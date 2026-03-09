package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ─────────────────────────────────────────────
// КОНСТАНТЫ
// ─────────────────────────────────────────────

const (
	PORT string = "8080"
)

// ─────────────────────────────────────────────
// ТИПЫ
// ─────────────────────────────────────────────

// Notification — структура уведомления, которое сервер шлёт клиенту.
// Поля с omitempty не попадают в JSON если пустые.
type Notification struct {
	Event   string `json:"event"`             // тип события: "connected", "peer_discovered", "custom"
	Message string `json:"message,omitempty"` // человекочитаемый текст
	PeerID  string `json:"peer_id,omitempty"` // кто триггернул событие
	PeerIP  string `json:"peer_ip,omitempty"` // публичный IP того, кто триггернул
}

// Client — один подключённый клиент.
type Client struct {
	id   string // уникальный ID (присваивается сервером при подключении)
	ip   string // публичный IP:Port клиента
	conn *websocket.Conn
	send chan Notification // буферизованный канал исходящих уведомлений
	once sync.Once         // гарантирует однократное закрытие канала send
}

// ─────────────────────────────────────────────
// СЕРВЕР
// ─────────────────────────────────────────────

type Server struct {
	clients  map[string]*Client // id -> Client
	mu       sync.RWMutex
	upgrader websocket.Upgrader
}

func NewServer() *Server {
	return &Server{
		clients: make(map[string]*Client),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

// ─────────────────────────────────────────────
// ОПРЕДЕЛЕНИЕ РЕАЛЬНОГО IP
// ─────────────────────────────────────────────

// realIP извлекает публичный IP клиента.
//
// Порядок проверки:
//  1. X-Forwarded-For   — если сервер за Nginx/HAProxy/CloudFlare
//  2. X-Real-IP         — альтернативный заголовок от Nginx
//  3. r.RemoteAddr      — прямое TCP-соединение (наш случай без прокси)
//
// Почему не только RemoteAddr?
//
//	Если между клиентом и сервером стоит reverse proxy (Nginx),
//	RemoteAddr будет IP самого прокси (127.0.0.1 или внутренний адрес).
//	Реальный IP клиента прокси кладёт в заголовок X-Forwarded-For.
func realIP(r *http.Request) string {
	// Случай 1: за прокси
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For может содержать цепочку: "clientIP, proxy1IP, proxy2IP"
		// Нас интересует самый первый — это и есть клиент
		if idx := len(xff); idx > 0 {
			// Берём до первой запятой
			for i, c := range xff {
				if c == ',' {
					return xff[:i]
				}
			}
		}
		return xff
	}

	// Случай 2: Nginx с proxy_set_header X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Случай 3: прямое подключение — разбираем "IP:port"
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// SplitHostPort упал — значит порта нет, возвращаем как есть
		return r.RemoteAddr
	}
	// Возвращаем вместе с портом — он нужен для UDP hole punching
	return net.JoinHostPort(host, port)
}

// ─────────────────────────────────────────────
// ЖИЗНЕННЫЙ ЦИКЛ КЛИЕНТА
// ─────────────────────────────────────────────

// connect принимает WebSocket-соединение, создаёт клиента и запускает циклы.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] upgrade failed: %v", err)
		return
	}

	ip := realIP(r)
	id := generateID() // короткий уникальный ID

	client := &Client{
		id:   id,
		ip:   ip,
		conn: conn,
		send: make(chan Notification, 32),
	}

	// Регистрируем клиента
	s.register(client)

	log.Printf("[server] client connected  id=%s ip=%s", id, ip)

	// Два независимых цикла — стандартный паттерн gorilla/websocket.
	// writePump пишет в соединение, readPump читает (и держит его живым).
	// Когда один завершается — завершается и второй через закрытие канала.
	go client.writePump()
	go client.readPump(s)
}

// register добавляет клиента в реестр и уведомляет его об успешном подключении.
func (s *Server) register(c *Client) {
	s.mu.Lock()
	s.clients[c.id] = c
	s.mu.Unlock()

	// Сразу сообщаем клиенту его собственный публичный IP
	c.send <- Notification{
		Event:   "connected",
		Message: "your public address is known",
		PeerID:  c.id,
		PeerIP:  c.ip,
	}
}

// unregister удаляет клиента из реестра при отключении.
func (s *Server) unregister(c *Client) {
	s.mu.Lock()
	delete(s.clients, c.id)
	s.mu.Unlock()

	// Закрываем канал — writePump увидит это и завершится
	c.once.Do(func() { close(c.send) })

	log.Printf("[server] client disconnected id=%s ip=%s", c.id, c.ip)
}

// ─────────────────────────────────────────────
// ЦИКЛЫ ЧТЕНИЯ И ЗАПИСИ
// ─────────────────────────────────────────────

// writePump — единственное место записи в WebSocket.
//
// Почему отдельная горутина?
//
//	gorilla/websocket запрещает конкурентную запись.
//	Все уведомления приходят в канал send, writePump
//	отправляет их строго по одному.
func (c *Client) writePump() {
	// Ping каждые 25 секунд — иначе NAT и прокси закроют "тихое" соединение
	ticker := time.NewTicker(25 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {

		case notif, ok := <-c.send:
			// Дедлайн на запись — не ждём вечно зависшего клиента
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			if !ok {
				// Канал закрыт (unregister вызвал close) — шлём Close frame
				c.conn.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
				)
				return
			}

			if err := c.conn.WriteJSON(notif); err != nil {
				log.Printf("[client %s] write error: %v", c.id, err)
				return
			}

		case <-ticker.C:
			// Keepalive ping
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				// Клиент не ответил — соединение мёртвое
				log.Printf("[client %s] ping failed: %v", c.id, err)
				return
			}
		}
	}
}

// readPump — держит соединение живым и читает входящие сообщения.
//
// Зачем читать, если нам не нужны данные от клиента?
//  1. gorilla требует активного чтения для обработки Pong (ответ на Ping)
//  2. Без чтения не узнаем о закрытии соединения со стороны клиента
//  3. Буфер входящих данных переполнится и соединение встанет
func (c *Client) readPump(s *Server) {
	defer s.unregister(c)

	c.conn.SetReadLimit(512) // клиент не должен слать нам много

	// 60 секунд тишины = соединение мёртвое
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Каждый Pong продлевает дедлайн
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		// Читаем, но не используем — нам важен сам факт чтения
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
			) {
				log.Printf("[client %s] unexpected close: %v", c.id, err)
			}
			return // выход → defer вызовет unregister
		}
		// Продлеваем дедлайн при любом входящем сообщении
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
}

// ─────────────────────────────────────────────
// ОТПРАВКА УВЕДОМЛЕНИЙ
// ─────────────────────────────────────────────

// Notify отправляет уведомление конкретному клиенту по его ID.
// Используй этот метод из любого места программы.
func (s *Server) Notify(clientID string, n Notification) bool {
	s.mu.RLock()
	c, ok := s.clients[clientID]
	s.mu.RUnlock()

	if !ok {
		return false // клиент не найден или уже отключился
	}

	select {
	case c.send <- n:
		return true
	default:
		// Буфер канала переполнен — клиент не успевает читать
		log.Printf("[server] send buffer full for client %s, dropping notification", clientID)
		return false
	}
}

// NotifyAll рассылает уведомление всем подключённым клиентам.
func (s *Server) NotifyAll(n Notification) {
	s.mu.RLock()
	// Копируем список ID под блокировкой, чтобы не держать её во время отправки
	ids := make([]string, 0, len(s.clients))
	for id := range s.clients {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		s.Notify(id, n)
	}
}

// NotifyAllExcept рассылает всем, кроме указанного клиента.
// Полезно когда клиент A появился — уведомить всех остальных.
func (s *Server) NotifyAllExcept(excludeID string, n Notification) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.clients))
	for id := range s.clients {
		if id != excludeID {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()

	for _, id := range ids {
		s.Notify(id, n)
	}
}

// Clients возвращает снимок текущего состояния: id -> ip.
// Снимок (копия), а не ссылка — потокобезопасно.
func (s *Server) Clients() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshot := make(map[string]string, len(s.clients))
	for id, c := range s.clients {
		snapshot[id] = c.ip
	}
	return snapshot
}

// ─────────────────────────────────────────────
// HTTP ОБРАБОТЧИКИ
// ─────────────────────────────────────────────

// handleWS — точка входа WebSocket соединений.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.connect(w, r)
}

// handleClients — REST endpoint: показывает всех подключённых клиентов и их IP.
// GET /clients → {"abc123":"1.2.3.4:5000", "def456":"5.6.7.8:9000"}
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Clients())
}

// handleNotify — REST endpoint для ручной отправки уведомления через HTTP.
// POST /notify?id=abc123
// Body: {"event":"custom","message":"hello"}
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	targetID := r.URL.Query().Get("id")
	if targetID == "" {
		http.Error(w, "missing ?id=", http.StatusBadRequest)
		return
	}

	var n Notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if ok := s.Notify(targetID, n); !ok {
		http.Error(w, "client not found or buffer full", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ─────────────────────────────────────────────
// ВСПОМОГАТЕЛЬНОЕ
// ─────────────────────────────────────────────

// generateID генерирует короткий уникальный ID на основе времени.
// В продакшне замени на github.com/google/uuid.
func generateID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// ─────────────────────────────────────────────
// ТОЧКА ВХОДА
// ─────────────────────────────────────────────

func main() {
	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS)           // WebSocket
	mux.HandleFunc("/clients", srv.handleClients) // смотреть кто подключён
	mux.HandleFunc("/notify", srv.handleNotify)   // слать уведомление вручную

	httpSrv := &http.Server{
		Addr:         ":" + PORT,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Println("signal server listening on ", PORT)
	log.Fatal(httpSrv.ListenAndServe())
}
