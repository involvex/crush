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
	"strings"
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
	// Map WebSocket session ID to current chat session ID
	webSessionToChatSession = make(map[string]string)
	webSessionMutex         sync.Mutex
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
				Type      string `json:"type"`
				Content   string `json:"content"`
				SessionID string `json:"sessionId"`
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
	Type      string `json:"type"`
	Content   string `json:"content"`
	SessionID string `json:"sessionId"`
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
		// Handle message processing in a separate goroutine
		go func() {
			// Subscribe to messages from the app
			messageEvents := appInstance.Messages.Subscribe(context.Background())

			// Run the agent coordinator
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
				return
			}

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
				case <-time.After(30 * time.Second): // Timeout after 30 seconds of no messages
					// Send HUD update for tokens after response is complete
					sendHUDUpdate()
					return
				case <-ctx.Done(): // Client disconnected
					slog.Info("Client disconnected during AI response streaming")
					return
				}
			}
		}()
	case "command":
		// Parse the command from content (e.g., "/cd /tmp" -> command="/cd", args=["/tmp"])
		parts := strings.Split(strings.TrimSpace(clientMsg.Content), " ")
		if len(parts) == 0 {
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = "Error: Empty command"
			sendResponse(clientMessageChan, serverResponse)
			return
		}

		command := parts[0]
		args := parts[1:]

		switch command {
		case "/cd":
			if len(args) == 0 {
				serverResponse.Type = "error"
				serverResponse.Sender = "System"
				serverResponse.Content = "Error: path is required for /cd command."
			} else {
				path := args[0]
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
			if len(args) == 0 {
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
				modelName := args[0]
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
			serverResponse.Content = fmt.Sprintf("Error: Unknown command '%s'", command)
		}
		sendResponse(clientMessageChan, serverResponse)
	case "get_commands":
		// Send available commands
		commandsResponse := struct {
			Type     string `json:"type"`
			Commands []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Type        string `json:"type"`
			} `json:"commands"`
		}{
			Type: "commands",
			Commands: []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Type        string `json:"type"`
			}{
				{ID: "new_session", Title: "New Session", Description: "Start a new session", Type: "system"},
				{ID: "switch_session", Title: "Switch Session", Description: "Switch to a different session", Type: "system"},
				{ID: "switch_model", Title: "Switch Model", Description: "Switch to a different model", Type: "system"},
				{ID: "summarize", Title: "Summarize Session", Description: "Summarize the current session", Type: "system"},
				{ID: "file_picker", Title: "Open File Picker", Description: "Open file picker", Type: "system"},
				{ID: "toggle_help", Title: "Toggle Help", Description: "Toggle help", Type: "system"},
				{ID: "quit", Title: "Quit", Description: "Quit application", Type: "system"},
			},
		}
		sendResponse(clientMessageChan, commandsResponse)
	case "get_models":
		// Send available models
		modelsResponse := struct {
			Type   string `json:"type"`
			Models []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Provider string `json:"provider"`
			} `json:"models"`
		}{
			Type: "models",
			Models: []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Provider string `json:"provider"`
			}{},
		}

		for p := range appInstance.Config().Providers.Seq() {
			for _, model := range p.Models {
				modelsResponse.Models = append(modelsResponse.Models, struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Provider string `json:"provider"`
				}{
					ID:       model.ID,
					Name:     model.ID,
					Provider: p.ID,
				})
			}
		}
		sendResponse(clientMessageChan, modelsResponse)
	case "get_sessions":
		// Send available sessions
		allSessions, err := appInstance.Sessions.List(context.Background())
		sessionsResponse := struct {
			Type     string `json:"type"`
			Sessions []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				CreatedAt string `json:"createdAt"`
			} `json:"sessions"`
		}{
			Type: "sessions",
			Sessions: []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				CreatedAt string `json:"createdAt"`
			}{},
		}

		if err == nil {
			for _, session := range allSessions {
				sessionsResponse.Sessions = append(sessionsResponse.Sessions, struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					CreatedAt string `json:"createdAt"`
				}{
					ID:        session.ID,
					Name:      session.Title,
					CreatedAt: time.Unix(session.CreatedAt, 0).Format(time.RFC3339),
				})
			}
		}
		sendResponse(clientMessageChan, sessionsResponse)
	case "switch_model":
		modelId := clientMsg.Content
		// Find the model in all providers
		var foundModel *config.SelectedModel
		for p := range appInstance.Config().Providers.Seq() {
			for _, model := range p.Models {
				if model.ID == modelId {
					foundModel = &config.SelectedModel{
						Model:    model.ID,
						Provider: p.ID,
					}
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
			serverResponse.Content = fmt.Sprintf("Error: Model '%s' not found.", modelId)
		} else {
			err := appInstance.Config().UpdatePreferredModel(config.SelectedModelTypeLarge, *foundModel)
			if err != nil {
				serverResponse.Type = "error"
				serverResponse.Sender = "System"
				serverResponse.Content = fmt.Sprintf("Error setting model: %v", err)
			} else {
				serverResponse.Type = "info"
				serverResponse.Sender = "System"
				serverResponse.Content = fmt.Sprintf("Switched to model: %s", foundModel.Model)
				sendHUDUpdate()
			}
		}
		sendResponse(clientMessageChan, serverResponse)
	case "switch_session":
		targetSessionId := clientMsg.Content

		// Verify the target session exists
		_, err := appInstance.Sessions.Get(context.Background(), targetSessionId)
		if err != nil {
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = fmt.Sprintf("Error: Session not found: %v", err)
			sendResponse(clientMessageChan, serverResponse)
			return
		}

		// Update the mapping from web session to chat session
		webSessionMutex.Lock()
		webSessionToChatSession[clientMsg.SessionID] = targetSessionId
		webSessionMutex.Unlock()

		// Update activeWebSessions to point to the target session
		sessionMutex.Lock()
		// Get the target session
		targetSess, err := appInstance.Sessions.Get(context.Background(), targetSessionId)
		if err == nil {
			activeWebSessions[clientMsg.SessionID] = &targetSess
		}
		sessionMutex.Unlock()

		serverResponse.Type = "info"
		serverResponse.Sender = "System"
		serverResponse.Content = "Switched to session"
		sendResponse(clientMessageChan, serverResponse)
	case "new_session":
		// Create a new session
		newSess, err := appInstance.Sessions.Create(context.Background(), "Web Chat Session")
		if err != nil {
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = fmt.Sprintf("Error creating session: %v", err)
		} else {
			// Add to active sessions
			sessionMutex.Lock()
			activeWebSessions[newSess.ID] = &newSess
			sessionMutex.Unlock()

			serverResponse.Type = "info"
			serverResponse.Sender = "System"
			serverResponse.Content = "Created new session"
		}
		sendResponse(clientMessageChan, serverResponse)
	case "execute_command":
		commandId := clientMsg.Content
		switch commandId {
		case "summarize":
			if sess != nil {
				err := appInstance.AgentCoordinator.Summarize(context.Background(), sess.ID)
				if err != nil {
					serverResponse.Type = "error"
					serverResponse.Sender = "System"
					serverResponse.Content = fmt.Sprintf("Error summarizing session: %v", err)
				} else {
					serverResponse.Type = "info"
					serverResponse.Sender = "System"
					serverResponse.Content = "Session summarized"
				}
			}
		default:
			serverResponse.Type = "error"
			serverResponse.Sender = "System"
			serverResponse.Content = fmt.Sprintf("Unknown command: %s", commandId)
		}
		sendResponse(clientMessageChan, serverResponse)
	case "get_files":
		path := clientMsg.Content
		if path == "" {
			path = "."
		}
		// For now, just return a simple file list
		// In a real implementation, you'd list files from the given path
		filesResponse := struct {
			Type  string `json:"type"`
			Files []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"files"`
		}{
			Type: "files",
			Files: []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			}{
				{Name: ".", Type: "directory"},
				{Name: "..", Type: "directory"},
				{Name: "example.txt", Type: "file"},
				{Name: "src", Type: "directory"},
			},
		}
		sendResponse(clientMessageChan, filesResponse)
	case "quit":
		// Handle quit
		serverResponse.Type = "info"
		serverResponse.Sender = "System"
		serverResponse.Content = "Goodbye!"
		sendResponse(clientMessageChan, serverResponse)
		// Close the connection
		return
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
