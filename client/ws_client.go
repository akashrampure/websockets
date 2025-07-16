package client

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type Message struct {
	Sender   string `json:"sender"`
	Receiver string `json:"receiver"`
	Data     []byte `json:"data"`
}

type MessageHandler func(msg Message)

type SubscribeConfig struct {
	Scheme string
	Host   string
	Port   string
	Path   string

	MaxRetries        int
	ReconnectInterval time.Duration
}

func NewSubscribeConfig(scheme, host, port, path string, maxRetries, reconnectInterval int) SubscribeConfig {
	reconnectTime := time.Duration(reconnectInterval) * time.Second

	return SubscribeConfig{
		Scheme:            scheme,
		Host:              host,
		Port:              port,
		Path:              path,
		MaxRetries:        maxRetries,
		ReconnectInterval: reconnectTime,
	}
}

type SubscribeWS struct {
	Config SubscribeConfig
	connMu sync.RWMutex
	logger *log.Logger
	conn   *websocket.Conn

	ClientID  string
	onReceive MessageHandler
}

func NewSubscribeWS(config SubscribeConfig, clientID string, logger *log.Logger) *SubscribeWS {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	return &SubscribeWS{
		Config:   config,
		ClientID: clientID,
		logger:   logger,
		connMu:   sync.RWMutex{},
	}
}

func (s *SubscribeWS) connectAndListen() error {
	url := s.Config.Scheme + "://" + s.Config.Host + ":" + s.Config.Port + s.Config.Path

	headers := http.Header{}
	headers.Add("Client-ID", s.ClientID)

	ws, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		s.logger.Printf("Initial connection failed: %v", err)
		return err
	}
	s.connMu.Lock()
	s.conn = ws
	s.connMu.Unlock()
	s.logger.Printf("Connected to %s", url)

	s.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	s.conn.SetPongHandler(func(appData string) error {
		s.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	defer s.conn.Close()

	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return err
		}

		var message Message
		if err := json.Unmarshal(raw, &message); err != nil {
			s.logger.Println("JSON unmarshal error:", err)
			continue
		}

		if s.onReceive != nil && message.Receiver == s.ClientID {
			s.onReceive(message)
		}
	}
}

func (s *SubscribeWS) SetOnReceive(handler MessageHandler) {
	s.onReceive = handler
}

func (s *SubscribeWS) Close() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func (s *SubscribeWS) SendMessage(receiver string, data interface{}) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()

	jsonData, err := json.Marshal(data)
	if err != nil {
		s.logger.Println("JSON marshal error:", err)
		return
	}

	message := Message{
		Sender:   s.ClientID,
		Receiver: receiver,
		Data:     jsonData,
	}
	if s.conn != nil {
		jsonMsg, err := json.Marshal(message)
		if err != nil {
			s.logger.Println("JSON marshal error:", err)
			return
		}
		if err := s.conn.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
			s.logger.Println("Write error:", err)
		}
	}
}

func (s *SubscribeWS) Start() {
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)

		<-sigint

		s.Close()
		s.logger.Println("Shutting down gracefully...")
		os.Exit(0)
	}()

	go func() {
		for i := 0; i < s.Config.MaxRetries; i++ {
			err := s.connectAndListen()
			if err != nil {
				s.logger.Printf("Connection lost: %v. Reconnecting (%d/%d)...", err, i+1, s.Config.MaxRetries)
				time.Sleep(s.Config.ReconnectInterval)
			} else {
				return
			}
		}

		s.Close()
		s.logger.Println("Max retries reached. Exiting...")
		os.Exit(1)
	}()
}
