package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/openclaw/whatsapp/bridge"
	"github.com/openclaw/whatsapp/store"
)

// Server holds the dependencies for all HTTP handlers.
type Server struct {
	Client   *bridge.Client
	Store    *store.MessageStore
	Log      *slog.Logger
	Version  string
}

// NewRouter returns a fully configured chi router with all API routes.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(corsMiddleware)
	r.Use(requestLogger(s.Log))

	// Status & auth
	r.Get("/status", s.handleStatus)
	r.Post("/logout", s.handleLogout)

	// QR web UI
	r.Get("/qr", s.handleQRPage)
	r.Get("/qr/data", s.handleQRData)

	// Messaging
	r.Post("/send/text", s.handleSendText)
	r.Post("/send/file", s.handleSendFile)
	r.Post("/reply", s.handleReply)
	r.Get("/messages", s.handleGetMessages)
	r.Get("/messages/search", s.handleSearchMessages)
	r.Post("/groups", s.handleCreateGroup)
	r.Post("/groups/join", s.handleJoinGroup)

	// Contacts & chats
	r.Get("/chats", s.handleGetChats)
	r.Get("/chats/{jid}/messages", s.handleGetChatMessages)
	r.Get("/contacts", s.handleGetContacts)

	return r
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// --- middleware --------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Debug("http request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			next.ServeHTTP(w, r)
		})
	}
}
