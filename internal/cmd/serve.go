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
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v3/cpu" // Added gopsutil import
	"github.com/shirou/gopsutil/v3/mem" // Added gopsutil import
	"github.com/spf13/cobra"
)

// SystemMetrics holds the collected system usage data.
type SystemMetrics struct {
	CPUUsage   float64 `json:"cpuUsage"`
	MemUsedMB  uint64  `json:"memUsedMB"`
	MemTotalMB uint64  `json:"memTotalMB"`
	MemUsage   float64 `json:"memUsage"`
}

// GetSystemMetrics collects CPU and memory usage.
func GetSystemMetrics() (SystemMetrics, error) {
	var metrics SystemMetrics

	// Get CPU usage
	cpuPercentages, err := cpu.Percent(time.Second, false)
	if err != nil {
		slog.Error("Error getting CPU percent", "error", err)
		return metrics, err
	}
	if len(cpuPercentages) > 0 {
		metrics.CPUUsage = cpuPercentages[0]
	}

	// Get memory usage
	v, err := mem.VirtualMemory()
	if err != nil {
		slog.Error("Error getting virtual memory", "error", err)
		return metrics, err
	}
	metrics.MemTotalMB = v.Total / 1024 / 1024
	metrics.MemUsedMB = v.Used / 1024 / 1024
	metrics.MemUsage = v.UsedPercent

	return metrics, nil
}

var (
	hostname          string
	port              int
	activeWebSessions = make(map[string]*session.Session)
	sessionMutex      sync.Mutex
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
	// Create a new app session for this web session
	sess, err := appInstance.Sessions.Create(context.Background(), "Web Chat Session")
	if err != nil {
		slog.Error("Failed to create app session for web client", "error", err)
		return
	}
	webSessionID := sess.ID

	sessionMutex.Lock()
	activeWebSessions[webSessionID] = &sess
	sessionMutex.Unlock()

	defer func() {
		sessionMutex.Lock()
		delete(activeWebSessions, webSessionID)
		sessionMutex.Unlock()
		slog.Info("Web session removed", "webSessionID", webSessionID)
	}()

	// Get initial HUD data
	currentModel := appInstance.Config().Models[config.SelectedModelTypeLarge].Model
	cwd, _ := os.Getwd()

	// Send the session ID and initial HUD data to the client
	initialMessage := struct {
		Type         string  `json:"type"`
		Sender       string  `json:"sender"`
		Content      string  `json:"content"`
		SessionID    string  `json:"sessionId"`
		CurrentModel string  `json:"currentModel"`
		CWD          string  `json:"cwd"`
		CPUUsage     float64 `json:"cpuUsage"`
		MemUsedMB    uint64  `json:"memUsedMB"`
		MemTotalMB   uint64  `json:"memTotalMB"`
		MemUsage     float64 `json:"memUsage"`
	}{
		Type:         "system",
		Sender:       "System",
		Content:      "Connected to Crush AI. Type /help for commands.",
		SessionID:    webSessionID,
		CurrentModel: currentModel,
		CWD:          cwd,
	}

	// Get initial system metrics
	sysMetrics, err := GetSystemMetrics()
	if err == nil {
		initialMessage.CPUUsage = sysMetrics.CPUUsage
		initialMessage.MemUsedMB = sysMetrics.MemUsedMB
		initialMessage.MemTotalMB = sysMetrics.MemTotalMB
		initialMessage.MemUsage = sysMetrics.MemUsage
	}

	if err := conn.WriteJSON(initialMessage); err != nil {
		slog.Error("Failed to send initial message with session ID and HUD data", "error", err)
		return
	}

	// Channel to send messages to the client
	clientMessageChan := make(chan []byte, 10)

	// Goroutine to send system metrics periodically
	go func() {
		ticker := time.NewTicker(5 * time.Second) // Update every 5 seconds
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sysMetrics, err := GetSystemMetrics()
				if err != nil {
					slog.Error("Failed to get system metrics for HUD update", "error", err)
					continue
				}
				hud := HUDUpdate{
					Type:             "hudUpdate",
					CurrentModel:     appInstance.Config().Models[config.SelectedModelTypeLarge].Model,
					CWD:              cwd, // CWD doesn't change unless /cd is used, but sending it for consistency
					PromptTokens:     sess.PromptTokens,
					CompletionTokens: sess.CompletionTokens,
					TotalTokens:      sess.PromptTokens + sess.CompletionTokens,
					CPUUsage:         sysMetrics.CPUUsage,
					MemUsedMB:        sysMetrics.MemUsedMB,
					MemTotalMB:       sysMetrics.MemTotalMB,
					MemUsage:         sysMetrics.MemUsage,
				}
				sendResponse(clientMessageChan, hud)
			case <-ctx.Done():
				slog.Info("System metrics goroutine stopped for client", "webSessionID", webSessionID)
				return
			}
		}
	}()

	// Reader Goroutine
	go func() {
		for {
			_, p, err := conn.ReadMessage()
			if err != nil {
				slog.Error("Failed to read WebSocket message", "error", err)
				return // Exit reader goroutine
			}

			var clientMsg struct {
				Type      string   `json:"type"`
				Content   string   `json:"content"`
				Command   string   `json:"command"`
				Args      []string `json:"args"`
				SessionID string   `json:"sessionId"` // Added SessionID field
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

type HUDUpdate struct {
	Type             string  `json:"type"`
	CurrentModel     string  `json:"currentModel"`
	CWD              string  `json:"cwd"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CPUUsage         float64 `json:"cpuUsage"`
	MemUsedMB        uint64  `json:"memUsedMB"`
	MemTotalMB       uint64  `json:"memTotalMB"`
	MemUsage         float64 `json:"memUsage"`
}

func processClientMessage(appInstance *app.App, conn *websocket.Conn, clientMsg struct {
	Type      string   `json:"type"`
	Content   string   `json:"content"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	SessionID string   `json:"sessionId"` // Added SessionID field
}, clientMessageChan chan<- []byte, ctx context.Context,
) {
	var serverResponse struct {
		Type    string `json:"type"`
		Sender  string `json:"sender"`
		Content string `json:"content"`
	}

	// Retrieve session based on clientMsg.SessionID
	sessionMutex.Lock()
	sess, ok := activeWebSessions[clientMsg.SessionID]
	sessionMutex.Unlock()

	if !ok {
		slog.Error("Web session not found", "webSessionID", clientMsg.SessionID)
		serverResponse.Type = "error"
		serverResponse.Sender = "System"
		serverResponse.Content = "Error: Web session not found. Please refresh."
		sendResponse(clientMessageChan, serverResponse)
		return
	}

	// Helper to send HUD updates
	sendHUDUpdate := func() {
		currentModel := appInstance.Config().Models[config.SelectedModelTypeLarge].Model
		cwd, _ := os.Getwd()
		sysMetrics, err := GetSystemMetrics()
		if err != nil {
			slog.Error("Failed to get system metrics for HUD update", "error", err)
			// Continue without system metrics if there's an error
		}
		hud := HUDUpdate{
			Type:             "hudUpdate",
			CurrentModel:     currentModel,
			CWD:              cwd,
			PromptTokens:     sess.PromptTokens,
			CompletionTokens: sess.CompletionTokens,
			TotalTokens:      sess.PromptTokens + sess.CompletionTokens,
			CPUUsage:         sysMetrics.CPUUsage,
			MemUsedMB:        sysMetrics.MemUsedMB,
			MemTotalMB:       sysMetrics.MemTotalMB,
			MemUsage:         sysMetrics.MemUsage,
		}
		sendResponse(clientMessageChan, hud)
	}

	switch clientMsg.Type {
	case "message":
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
			// After AI response, send HUD update for tokens
			sendHUDUpdate()
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
					sendHUDUpdate() // Send HUD update after CWD changes
				}
			}
		case "/help":
			serverResponse.Type = "info"
			serverResponse.Sender = "System"
			serverResponse.Content = "Available commands: /cd <path>, /help, /model"
		case "/model":
			if len(clientMsg.Args) == 0 {
				// List all available models from all providers
				modelList := "Available models:\n"
				uniqueModels := make(map[string]struct{}) // To store unique model names
				for p := range appInstance.Config().Providers.Seq() {
					for _, model := range p.Models {
						modelIdentifier := fmt.Sprintf("%s (Provider: %s)", model.ID, p.ID)
						if _, exists := uniqueModels[modelIdentifier]; !exists {
							modelList += fmt.Sprintf("- %s\n", modelIdentifier)
							uniqueModels[modelIdentifier] = struct{}{}
						}
					}
				}
				serverResponse.Type = "info"
				serverResponse.Sender = "System"
				serverResponse.Content = modelList
			} else {
				// Try to set preferred model
				modelName := clientMsg.Args[0]
				// Find the model in all providers
				var foundModel *config.SelectedModel
				var foundProviderID string
				for p := range appInstance.Config().Providers.Seq() {
					for _, model := range p.Models {
						if model.ID == modelName {
							foundModel = &config.SelectedModel{
								Model:    model.ID,
								Provider: p.ID,
							}
							foundProviderID = p.ID
							break
						}
					}
					if foundModel != nil {
						break
					}
				}

				if foundModel == nil {
					serverResponse.Type = "error"
					serverResponse.Sender = "System"
					serverResponse.Content = fmt.Sprintf("Error: Model '%s' not found in any provider.", modelName)
				} else {
					// For simplicity, set the selected model as the new "large" model
					err := appInstance.Config().UpdatePreferredModel(config.SelectedModelTypeLarge, *foundModel)
					if err != nil {
						serverResponse.Type = "error"
						serverResponse.Sender = "System"
						serverResponse.Content = fmt.Sprintf("Error setting preferred model: %v", err)
					} else {
						serverResponse.Type = "info"
						serverResponse.Sender = "System"
						serverResponse.Content = fmt.Sprintf("Preferred model set to %s (Provider: %s)", foundModel.Model, foundProviderID)
						sendHUDUpdate() // Send HUD update after model changes
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
