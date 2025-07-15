package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var serverOnce sync.Once

type Message struct {
	Sender   string `json:"sender"`
	Receiver string `json:"receiver"`
	Data     []byte `json:"data"`
}

type MessageHandler func(message Message)

type WebSocketConfig struct {
	Port            string
	Path            string
	ReadBufferSize  int
	WriteBufferSize int
	AllowedOrigins  []string
}

func NewWebSocketConfig(port, path string) WebSocketConfig {
	return WebSocketConfig{
		Port:            port,
		Path:            path,
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		AllowedOrigins:  []string{"*"},
	}
}

type WebSocketServer struct {
	Config        WebSocketConfig
	Upgrader      *websocket.Upgrader
	connections   map[string]*websocket.Conn
	connectionsMu sync.RWMutex
	onReceive     MessageHandler
	onConnect     func(clientID string)
	onDisconnect  func(clientID string, err error)
	logger        *log.Logger
	Message       Message
}

func NewWebSocketServer(config WebSocketConfig, logger *log.Logger) *WebSocketServer {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	return &WebSocketServer{
		Config: config,
		Upgrader: &websocket.Upgrader{
			ReadBufferSize:  config.ReadBufferSize,
			WriteBufferSize: config.WriteBufferSize,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		connections:  make(map[string]*websocket.Conn),
		onReceive:    func(message Message) {},
		onConnect:    func(clientID string) {},
		onDisconnect: func(clientID string, err error) {},
		logger:       logger,
		Message:      Message{},
	}
}

func (s *WebSocketServer) OnReceive(handler MessageHandler) {
	s.onReceive = handler

}

func (s *WebSocketServer) OnConnect(fn func(clientID string)) {
	s.onConnect = fn
}

func (s *WebSocketServer) OnDisconnect(fn func(clientID string, err error)) {
	s.onDisconnect = fn
}

func (s *WebSocketServer) RelayMessage(message Message) error {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()

	receiver := message.Receiver
	conn, exists := s.connections[receiver]
	if !exists {
		return errors.New("receiver not found")
	}

	msgBytes, err := json.Marshal(message)
	if err != nil {
		s.logger.Printf("Error marshalling message: %v\n", err)
		return err
	}
	err = conn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		s.logger.Printf("Error writing message: %v\n", err)
		return err
	}
	s.logger.Printf("Message relayed to %s\n", receiver)
	return nil
}

func (s *WebSocketServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := s.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Println("upgrade error:", err)
		return
	}

	clientID := r.Header.Get("Client-ID")
	if clientID == "" {
		s.logger.Println("missing Client-ID header")
		conn.Close()
		return
	}

	s.connectionsMu.Lock()
	s.connections[clientID] = conn
	s.connectionsMu.Unlock()

	if s.onConnect != nil {
		s.onConnect(clientID)
	}

	conn.SetReadLimit(1024)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					s.logger.Printf("ping error to %s: %v\n", clientID, err)
					return
				}
			case <-done:
				return
			}
		}
	}()

	defer func() {
		close(done)

		s.connectionsMu.Lock()
		delete(s.connections, clientID)
		s.connectionsMu.Unlock()

		if s.onDisconnect != nil {
			s.onDisconnect(clientID, err)
		}

		conn.Close()
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			s.logger.Printf("read error from %s: %v\n", clientID, err)
			break
		}

		var message Message
		err = json.Unmarshal(msg, &message)
		if err != nil {
			s.logger.Printf("error unmarshalling message: %v\n", err)
			break
		}

		if s.onReceive != nil {
			s.onReceive(message)
		}
	}
}

func (s *WebSocketServer) Start() {
	serverOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc(s.Config.Path, s.wsHandler)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":"ok"}`))
		})

		srv := &http.Server{
			Addr:         fmt.Sprintf(":%s", s.Config.Port),
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}

		s.OnConnect(func(clientID string) {
			s.logger.Printf("Connected: %s\n", clientID)
		})

		s.OnDisconnect(func(clientID string, err error) {
			if err != nil {
				s.logger.Printf("Error disconnecting: %s: %v\n", clientID, err)
			} else {
				s.logger.Printf("Disconnected: %s\n", clientID)
			}

			s.connectionsMu.RLock()
			noClients := len(s.connections) == 0
			s.connectionsMu.RUnlock()

			if noClients {
				s.logger.Println("No clients connected, shutting down server in 5 seconds...")
				time.Sleep(5 * time.Second)
				os.Exit(0)
			}
		})

		s.OnReceive(func(message Message) {
			fmt.Printf("Received message from %s to %s: %s\n", message.Sender, message.Receiver, string(message.Data))
			if err := s.RelayMessage(message); err != nil {
				s.logger.Printf("Error relaying message: %v\n", err)
			}
		})

		go func() {
			if err := srv.ListenAndServe(); err != nil {
				s.logger.Println("server closed:", err)
			}
		}()
	})
}
