package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

var (
	hostname string
	port     int
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web server",
	RunE: func(cmd *cobra.Command, args []string) error {
		appInstance, err := SetupAppOnly(cmd)
		if err != nil {
			return err
		}
		defer appInstance.Shutdown()

		addr := fmt.Sprintf("%s:%d", hostname, port)
		slog.Info("Starting web server", "address", addr)

		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			tmpl, err := template.ParseFiles("web/index.html")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmpl.Execute(w, nil)
		})

		http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				slog.Error("Failed to upgrade connection to WebSocket", "error", err)
				return
			}
			defer conn.Close()

			slog.Info("WebSocket client connected")
			handleClientConnection(appInstance, conn, r.Context())
			slog.Info("WebSocket client disconnected")
		})

		server := &http.Server{Addr: addr}

		// Handle graceful shutdown
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Failed to start web server", "error", err)
			}
		}()
		slog.Info("Web server started")

		<-done
		slog.Info("Shutting down web server...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			slog.Error("Web server shutdown failed", "error", err)
			return err
		}
		slog.Info("Web server gracefully stopped")
		return nil
	},
}

func handleClientConnection(appInstance *app.App, conn *websocket.Conn, ctx context.Context) {
	// Channel to send messages to the client
	clientMessageChan := make(chan []byte, 10)

	// Reader Goroutine
	go func() {
		for {
			_, p, err := conn.ReadMessage()
			if err != nil {
				slog.Error("Failed to read WebSocket message", "error", err)
				return // Exit reader goroutine
			}

			var clientMsg struct {
				Type    string   `json:"type"`
				Content string   `json:"content"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			}
			if err := json.Unmarshal(p, &clientMsg); err != nil {
				slog.Error("Failed to unmarshal client message", "error", err)
				continue
			}

			// Process client message
			processClientMessage(appInstance, conn, clientMsg, clientMessageChan, ctx)
		}
	}()

	// Writer Goroutine
	for {
		select {
		case msg := <-clientMessageChan:
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Error("Failed to write WebSocket message", "error", err)
				return // Exit writer goroutine
			}
		case <-ctx.Done():
			slog.Info("Client connection context cancelled")
			return // Exit writer goroutine
		}
	}
}

func processClientMessage(appInstance *app.App, conn *websocket.Conn, clientMsg struct {
	Type    string   `json:"type"`
	Content string   `json:"content"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}, clientMessageChan chan<- []byte, ctx context.Context,
) {
	var serverResponse struct {
		Type    string `json:"type"`
		Sender  string `json:"sender"`
		Content string `json:"content"`
	}

	switch clientMsg.Type {
	case "message":
		// Create a new session for each chat message for now.
		// TODO: Implement session management for web.
		sess, err := appInstance.Sessions.Create(context.Background(), "Web Chat Session")
		if err != nil {
			slog.Error("Failed to create session", "error", err)
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = fmt.Sprintf("Error creating session: %v", err)
			sendResponse(clientMessageChan, serverResponse)
			return
		}

		// Subscribe to messages from the app
		messageEvents := appInstance.Messages.Subscribe(context.Background())

		go func() {
			_, err := appInstance.AgentCoordinator.Run(context.Background(), sess.ID, clientMsg.Content)
			if err != nil {
				slog.Error("AgentCoordinator.Run failed", "error", err)
				// Send error back to client
				errorResponse := struct {
					Type    string `json:"type"`
					Sender  string `json:"sender"`
					Content string `json:"content"`
				}{
					Type:    "error",
					Sender:  "System",
					Content: fmt.Sprintf("AI Error: %v", err),
				}
				sendResponse(clientMessageChan, errorResponse)
			}
		}()

		// Stream messages back to the client
		for {
			select {
			case event := <-messageEvents:
				msg := event.Payload
				if msg.SessionID == sess.ID && msg.Role == message.Assistant && len(msg.Parts) > 0 {
					content := msg.Content().String()
					// TODO: Handle delta updates
					chatResponse := struct {
						Type    string `json:"type"`
						Sender  string `json:"sender"`
						Content string `json:"content"`
					}{
						Type:    "chat",
						Sender:  "AI",
						Content: content,
					}
					sendResponse(clientMessageChan, chatResponse)
				}
			case <-ctx.Done(): // Client disconnected
				slog.Info("Client disconnected during AI response streaming")
				return // Exit goroutine
			}
		}
	case "command":
		switch clientMsg.Command {
		case "/cd":
			if len(clientMsg.Args) == 0 {
				serverResponse.Type = "error"
				serverResponse.Sender = "System"
				serverResponse.Content = "Error: path is required for /cd command."
			} else {
				path := clientMsg.Args[0]
				err := appInstance.Chdir(path)
				if err != nil {
					serverResponse.Type = "error"
					serverResponse.Sender = "System"
					serverResponse.Content = fmt.Sprintf("Error changing directory: %v", err)
				} else {
					newCwd, _ := os.Getwd()
					serverResponse.Type = "info"
					serverResponse.Sender = "System"
					serverResponse.Content = fmt.Sprintf("Directory changed to %s", newCwd)
				}
			}
		case "/help":
			serverResponse.Type = "info"
			serverResponse.Sender = "System"
			serverResponse.Content = "Available commands: /cd <path>, /help, /model"
		case "/model":
			if len(clientMsg.Args) == 0 {
				// List available models
				models := appInstance.Config().Models
				modelList := "Available models:\n"
				for _, model := range models {
					modelList += fmt.Sprintf("- %s (Provider: %s)\n", model.Model, model.Provider)
				}
				serverResponse.Type = "info"
				serverResponse.Sender = "System"
				serverResponse.Content = modelList
			} else {
				// Try to set preferred model
				modelName := clientMsg.Args[0]
				selectedModel, ok := appInstance.Config().Models[config.SelectedModelType(modelName)]
				if !ok {
					serverResponse.Type = "error"
					serverResponse.Sender = "System"
					serverResponse.Content = fmt.Sprintf("Error: Model '%s' not found.", modelName)
				} else {
					err := appInstance.Config().UpdatePreferredModel(appInstance.Config().Agents["coder"].Model, selectedModel)
					if err != nil {
						serverResponse.Type = "error"
						serverResponse.Sender = "System"
						serverResponse.Content = fmt.Sprintf("Error setting preferred model: %v", err)
					} else {
						serverResponse.Type = "info"
						serverResponse.Sender = "System"
						serverResponse.Content = fmt.Sprintf("Preferred model set to %s", modelName)
					}
				}
			}
		default:
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = fmt.Sprintf("Error: Unknown command '%s'", clientMsg.Command)
		}
		sendResponse(clientMessageChan, serverResponse)
	default:
		serverResponse.Type = "error"
		serverResponse.Sender = "System"
		serverResponse.Content = fmt.Sprintf("Error: Unknown message type '%s'", clientMsg.Type)
		sendResponse(clientMessageChan, serverResponse)
	}
}

func sendResponse(clientMessageChan chan<- []byte, response interface{}) {
	respBytes, err := json.Marshal(response)
	if err != nil {
		slog.Error("Failed to marshal server response", "error", err)
		return
	}
	clientMessageChan <- respBytes
}

func init() {
	serveCmd.Flags().StringVar(&hostname, "hostname", "127.0.0.1", "Hostname to listen on")
	serveCmd.Flags().IntVar(&port, "port", 3000, "Port to listen on")
}
