package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"golang.org/x/oauth2"
)

//go:embed templates/*
var templateFS embed.FS

const (
	defaultListenAddr = ":8080"
	sessionCookieName = "motd_session"
	stateCookieName   = "discord_oauth_state"
	settingsFileName  = "data/settings.json"
)

type appConfig struct {
	ListenAddr        string
	RemoteEndpoint    string
	RemoteAPIKey      string
	RemoteAPISecret   string
	DiscordClientID   string
	DiscordSecret     string
	DiscordRedirect   string
	SessionSecret     string
	AllowedDiscordIDs map[string]struct{}
}

type settings struct {
	Hour                int         `json:"hour"`
	Minute              int         `json:"minute"`
	Timezone            string      `json:"timezone"`
	Messages            messageList `json:"messages"`
	RecentMessages      []string    `json:"recentMessages,omitempty"`
	LastRunAt           string      `json:"lastRunAt,omitempty"`
	LastMessage         string      `json:"lastMessage,omitempty"`
	LastScheduledRunAt  string      `json:"lastScheduledRunAt,omitempty"`
}

type messageEntry struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type messageList []messageEntry

type settingsSaveOperation struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
}

type settingsStore struct {
	path string
	mu   sync.RWMutex
	data settings
}

type remoteClient struct {
	endpoint  string
	apiKey    string
	apiSecret string
	client    *http.Client
}

type scheduler struct {
	store  *settingsStore
	remote *remoteClient
	logger *log.Logger
	mu     sync.Mutex
}

type app struct {
	cfg       appConfig
	store     *settingsStore
	scheduler *scheduler
	templates *template.Template
	oauth     *oauth2.Config
	logger    *log.Logger
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar"`
}

type dashboardData struct {
	User         discordUser
	AvatarURL    string
	Settings     settings
	MessagesJSON template.JS
	AllowedUsers []string
	Flash        string
	NextRunAt    string
}

type settingsSaveRequest struct {
	ScheduleTime    string                  `json:"scheduleTime"`
	BrowserTimezone string                  `json:"browserTimezone"`
	Operations      []settingsSaveOperation `json:"operations"`
}

type settingsSaveResponse struct {
	OK       bool     `json:"ok"`
	Message  string   `json:"message,omitempty"`
	Settings settings `json:"settings"`
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)

	cfg, err := loadAppConfig()
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	store, err := newSettingsStore(settingsFileName)
	if err != nil {
		logger.Fatalf("init settings store: %v", err)
	}

	tpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		logger.Fatalf("parse templates: %v", err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     cfg.DiscordClientID,
		ClientSecret: cfg.DiscordSecret,
		RedirectURL:  cfg.DiscordRedirect,
		Scopes:       []string{"identify"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://discord.com/oauth2/authorize",
			TokenURL: "https://discord.com/api/oauth2/token",
		},
	}

	remote := &remoteClient{
		endpoint:  cfg.RemoteEndpoint,
		apiKey:    cfg.RemoteAPIKey,
		apiSecret: cfg.RemoteAPISecret,
		client:    &http.Client{Timeout: 20 * time.Second},
	}

	s := &scheduler{
		store:  store,
		remote: remote,
		logger: logger,
	}

	application := &app{
		cfg:       cfg,
		store:     store,
		scheduler: s,
		templates: tpl,
		oauth:     oauthCfg,
		logger:    logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", application.handleDashboard)
	mux.HandleFunc("/auth/discord/login", application.handleDiscordLogin)
	mux.HandleFunc("/auth/discord/callback", application.handleDiscordCallback)
	mux.HandleFunc("/logout", application.handleLogout)
	mux.HandleFunc("/settings", application.handleSettingsSave)
	mux.HandleFunc("/run-now", application.handleRunNow)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           application.logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Printf("listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("serve: %v", err)
	}
}

func loadAppConfig() (appConfig, error) {
	cfg := appConfig{
		ListenAddr:      getenv("MOTD_LISTEN_ADDR", defaultListenAddr),
		RemoteEndpoint:  strings.TrimSpace(os.Getenv("MOTD_REMOTE_ENDPOINT")),
		RemoteAPIKey:    strings.TrimSpace(os.Getenv("MOTD_REMOTE_API_KEY")),
		RemoteAPISecret: strings.TrimSpace(os.Getenv("MOTD_REMOTE_API_SECRET")),
		DiscordClientID: strings.TrimSpace(os.Getenv("MOTD_DISCORD_CLIENT_ID")),
		DiscordSecret:   strings.TrimSpace(os.Getenv("MOTD_DISCORD_CLIENT_SECRET")),
		DiscordRedirect: strings.TrimSpace(os.Getenv("MOTD_DISCORD_REDIRECT_URL")),
		SessionSecret:   strings.TrimSpace(os.Getenv("MOTD_SESSION_SECRET")),
	}

	allowedIDs := strings.Split(strings.TrimSpace(os.Getenv("MOTD_ALLOWED_DISCORD_IDS")), ",")
	cfg.AllowedDiscordIDs = make(map[string]struct{}, len(allowedIDs))
	for _, id := range allowedIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		cfg.AllowedDiscordIDs[id] = struct{}{}
	}

	switch {
	case cfg.RemoteEndpoint == "":
		return cfg, errors.New("MOTD_REMOTE_ENDPOINT is required")
	case cfg.RemoteAPIKey == "":
		return cfg, errors.New("MOTD_REMOTE_API_KEY is required")
	case cfg.RemoteAPISecret == "":
		return cfg, errors.New("MOTD_REMOTE_API_SECRET is required")
	case cfg.DiscordClientID == "":
		return cfg, errors.New("MOTD_DISCORD_CLIENT_ID is required")
	case cfg.DiscordSecret == "":
		return cfg, errors.New("MOTD_DISCORD_CLIENT_SECRET is required")
	case cfg.DiscordRedirect == "":
		return cfg, errors.New("MOTD_DISCORD_REDIRECT_URL is required")
	case cfg.SessionSecret == "":
		return cfg, errors.New("MOTD_SESSION_SECRET is required")
	case len(cfg.AllowedDiscordIDs) == 0:
		return cfg, errors.New("MOTD_ALLOWED_DISCORD_IDS is required")
	}

	return cfg, nil
}

func newSettingsStore(path string) (*settingsStore, error) {
	store := &settingsStore{
		path: path,
		data: settings{
			Hour:     8,
			Minute:   0,
			Timezone: "UTC",
			Messages: messageList{{ID: newMessageID(), Text: "Welcome to the server."}},
		},
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&store.data); err != nil {
		return nil, err
	}

	store.normalizeLocked()
	return store, nil
}

func (s *settingsStore) get() settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSettings(s.data)
}

func (s *settingsStore) update(next settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = next
	s.normalizeLocked()
	return s.saveLocked()
}

func (s *settingsStore) applySave(hour, minute int, timezone string, operations []settingsSaveOperation) (settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.Hour = hour
	s.data.Minute = minute
	s.data.Timezone = strings.TrimSpace(timezone)

	for _, op := range operations {
		s.applyMessageOperationLocked(op)
	}

	s.normalizeLocked()
	if err := s.saveLocked(); err != nil {
		return settings{}, err
	}

	return cloneSettings(s.data), nil
}

func (s *settingsStore) recordRun(ts time.Time, message string, scheduledAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastRunAt = ts.Format(time.RFC3339)
	s.data.LastMessage = message
	if !scheduledAt.IsZero() {
		s.data.LastScheduledRunAt = scheduledAt.Format(time.RFC3339)
	}
	s.data.RecentMessages = append([]string{message}, s.data.RecentMessages...)
	if len(s.data.RecentMessages) > 5 {
		s.data.RecentMessages = s.data.RecentMessages[:5]
	}
	return s.saveLocked()
}

func (s *settingsStore) normalizeLocked() {
	if s.data.Hour < 0 || s.data.Hour > 23 {
		s.data.Hour = 8
	}
	if s.data.Minute < 0 || s.data.Minute > 59 {
		s.data.Minute = 0
	}
	if strings.TrimSpace(s.data.Timezone) == "" {
		s.data.Timezone = "UTC"
	}
	if _, err := loadScheduleLocation(s.data.Timezone); err != nil {
		s.data.Timezone = "UTC"
	}

	filtered := s.data.Messages[:0]
	seen := make(map[string]struct{}, len(s.data.Messages))
	for _, msg := range s.data.Messages {
		msg.ID = strings.TrimSpace(msg.ID)
		msg.Text = strings.TrimSpace(msg.Text)
		if msg.Text == "" {
			continue
		}
		if msg.ID == "" {
			msg.ID = newMessageID()
		}
		if _, ok := seen[msg.ID]; ok {
			msg.ID = newMessageID()
		}
		seen[msg.ID] = struct{}{}
		filtered = append(filtered, msg)
	}
	if len(filtered) == 0 {
		filtered = messageList{{ID: newMessageID(), Text: "Welcome to the server."}}
	}
	s.data.Messages = filtered

	allowed := make(map[string]struct{}, len(s.data.Messages))
	for _, msg := range s.data.Messages {
		allowed[msg.Text] = struct{}{}
	}

	recent := s.data.RecentMessages[:0]
	for _, msg := range s.data.RecentMessages {
		msg = strings.TrimSpace(msg)
		if msg == "" {
			continue
		}
		if _, ok := allowed[msg]; ok {
			recent = append(recent, msg)
		}
		if len(recent) == 5 {
			break
		}
	}
	s.data.RecentMessages = recent
}

func (s *settingsStore) saveLocked() error {
	payload, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, payload, 0o644)
}

func (s *settingsStore) applyMessageOperationLocked(op settingsSaveOperation) {
	id := strings.TrimSpace(op.ID)
	switch strings.ToLower(strings.TrimSpace(op.Type)) {
	case "delete":
		if id == "" {
			return
		}
		for i, msg := range s.data.Messages {
			if msg.ID != id {
				continue
			}
			s.data.Messages = append(s.data.Messages[:i], s.data.Messages[i+1:]...)
			return
		}
	case "add", "update":
		text := strings.TrimSpace(op.Text)
		if text == "" {
			return
		}
		if id == "" {
			id = newMessageID()
		}
		for i, msg := range s.data.Messages {
			if msg.ID != id {
				continue
			}
			s.data.Messages[i].Text = text
			return
		}
		s.data.Messages = append([]messageEntry{{ID: id, Text: text}}, s.data.Messages...)
	}
}

func (m *messageList) UnmarshalJSON(data []byte) error {
	var entries []messageEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		*m = append((*m)[:0], entries...)
		return nil
	}

	var legacy []string
	if err := json.Unmarshal(data, &legacy); err == nil {
		converted := make([]messageEntry, 0, len(legacy))
		for _, text := range legacy {
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			converted = append(converted, messageEntry{
				ID:   newMessageID(),
				Text: text,
			})
		}
		*m = converted
		return nil
	}

	return errors.New("invalid messages payload")
}

func newMessageID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func cloneSettings(in settings) settings {
	out := in
	out.Messages = append(messageList(nil), in.Messages...)
	out.RecentMessages = append([]string(nil), in.RecentMessages...)
	return out
}

func (s *scheduler) run(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			now := time.Now()
			scheduledAt := s.lastScheduledTimeFrom(now)
			wait := time.Until(s.nextRunFrom(now))
			if s.shouldRunScheduled(now, scheduledAt) {
				if err := s.execute(ctx, scheduledAt); err != nil {
					s.logger.Printf("scheduled run failed: %v", err)
				}
				wait = time.Until(s.nextRunFrom(time.Now().Add(time.Second)))
			}
			if wait < time.Second {
				wait = time.Second
			}
			timer.Reset(wait)
		}
	}
}

func (s *scheduler) nextRunFrom(now time.Time) time.Time {
	cfg := s.store.get()
	loc, err := loadScheduleLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	next := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), cfg.Hour, cfg.Minute, 0, 0, loc)
	if !next.After(localNow) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *scheduler) lastScheduledTimeFrom(now time.Time) time.Time {
	cfg := s.store.get()
	loc, err := loadScheduleLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	last := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), cfg.Hour, cfg.Minute, 0, 0, loc)
	if last.After(localNow) {
		last = last.AddDate(0, 0, -1)
	}
	return last
}

func (s *scheduler) shouldRunScheduled(now, scheduledAt time.Time) bool {
	if scheduledAt.IsZero() || scheduledAt.After(now) {
		return false
	}

	lastScheduledRunAt, ok := s.lastScheduledRunAt()
	if !ok {
		return true
	}

	return lastScheduledRunAt.Before(scheduledAt)
}

func (s *scheduler) lastScheduledRunAt() (time.Time, bool) {
	cfg := s.store.get()
	if strings.TrimSpace(cfg.LastScheduledRunAt) == "" {
		return time.Time{}, false
	}

	lastScheduledRunAt, err := time.Parse(time.RFC3339, cfg.LastScheduledRunAt)
	if err != nil {
		return time.Time{}, false
	}

	return lastScheduledRunAt, true
}

func (s *scheduler) execute(ctx context.Context, scheduledAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.store.get()
	message, err := pickRandomMessage(cfg.Messages, cfg.RecentMessages)
	if err != nil {
		return err
	}

	if err := s.remote.updateWelcomeMessage(ctx, message); err != nil {
		return err
	}

	return s.store.recordRun(time.Now(), message, scheduledAt)
}

func pickRandomMessage(messages messageList, recent []string) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("no messages configured")
	}

	candidates := filterMessages(messages, recent[:min(len(recent), 5)])
	if len(candidates) == 0 {
		candidates = filterMessages(messages, recent[:min(len(recent), 3)])
	}
	if len(candidates) == 0 {
		candidates = append([]string(nil), messageTexts(messages)...)
	}

	max := big.NewInt(int64(len(candidates)))
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return candidates[n.Int64()], nil
}

func filterMessages(messages messageList, excluded []string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, msg := range excluded {
		blocked[msg] = struct{}{}
	}

	var filtered []string
	for _, msg := range messageTexts(messages) {
		if _, ok := blocked[msg]; ok {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func messageTexts(messages messageList) []string {
	texts := make([]string, 0, len(messages))
	for _, msg := range messages {
		texts = append(texts, msg.Text)
	}
	return texts
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *remoteClient) updateWelcomeMessage(ctx context.Context, message string) error {
	configBody, err := c.fetchConfig(ctx)
	if err != nil {
		return err
	}

	var payload map[string]any
	if err := json.Unmarshal(configBody, &payload); err != nil {
		return fmt.Errorf("decode remote config: %w", err)
	}

	payload["WelcomeMessage"] = message

	updatedBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode remote config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.endpoint, bytes.NewReader(updatedBody))
	if err != nil {
		return err
	}
	c.applyHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PUT %s failed: %s: %s", c.endpoint, resp.Status, strings.TrimSpace(string(body)))
	}

	return nil
}

func (c *remoteClient) fetchConfig(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s failed: %s: %s", c.endpoint, resp.Status, strings.TrimSpace(string(body)))
	}

	return io.ReadAll(resp.Body)
}

func (c *remoteClient) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("X-API-Secret", c.apiSecret)
}

func (a *app) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	ids := make([]string, 0, len(a.cfg.AllowedDiscordIDs))
	for id := range a.cfg.AllowedDiscordIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	currentSettings := a.store.get()
	messagesJSON, err := json.Marshal(currentSettings.Messages)
	if err != nil {
		http.Error(w, "failed to prepare dashboard", http.StatusInternalServerError)
		return
	}

	data := dashboardData{
		User:         user,
		AvatarURL:    discordAvatarURL(user),
		Settings:     currentSettings,
		MessagesJSON: template.JS(messagesJSON),
		AllowedUsers: ids,
		Flash:        r.URL.Query().Get("flash"),
		NextRunAt:    a.scheduler.nextRunFrom(time.Now()).Format(time.RFC3339),
	}
	a.renderTemplate(w, "dashboard.html", data)
}

func (a *app) handleDiscordLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomHex(24)
	if err != nil {
		http.Error(w, "failed to create auth state", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(a.cfg.DiscordRedirect),
		MaxAge:   300,
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state), http.StatusFound)
}

func (a *app) handleDiscordCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "discord token exchange failed", http.StatusBadGateway)
		return
	}

	client := a.oauth.Client(r.Context(), token)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://discord.com/api/users/@me", nil)
	if err != nil {
		http.Error(w, "failed to create user request", http.StatusInternalServerError)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch discord profile", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "discord profile request failed", http.StatusBadGateway)
		return
	}

	var user discordUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		http.Error(w, "failed to decode discord profile", http.StatusBadGateway)
		return
	}

	if _, ok := a.cfg.AllowedDiscordIDs[user.ID]; !ok {
		http.Error(w, "your Discord account is not allowed", http.StatusForbidden)
		return
	}

	if err := a.writeSession(w, user); err != nil {
		http.Error(w, "failed to set session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/?flash="+url.QueryEscape("Signed in successfully."), http.StatusFound)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/auth/discord/login", http.StatusFound)
}

func (a *app) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, settingsSaveResponse{
			OK:      false,
			Message: "method not allowed",
		})
		return
	}
	if _, ok := a.requireUser(w, r); !ok {
		return
	}

	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, settingsSaveResponse{
			OK:      false,
			Message: "content type must be application/json",
		})
		return
	}

	var req settingsSaveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsSaveResponse{
			OK:      false,
			Message: "invalid json payload",
		})
		return
	}

	hour, minute, err := parseScheduleTime(req.ScheduleTime)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, settingsSaveResponse{
			OK:      false,
			Message: "invalid schedule time",
		})
		return
	}
	browserTimezone := strings.TrimSpace(req.BrowserTimezone)
	if browserTimezone == "" {
		writeJSON(w, http.StatusBadRequest, settingsSaveResponse{
			OK:      false,
			Message: "browser timezone is required",
		})
		return
	}

	if _, err := loadScheduleLocation(browserTimezone); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsSaveResponse{
			OK:      false,
			Message: "invalid browser timezone",
		})
		return
	}

	currentSettings, err := a.store.applySave(hour, minute, browserTimezone, req.Operations)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, settingsSaveResponse{
			OK:      false,
			Message: "failed to save settings",
		})
		return
	}

	writeJSON(w, http.StatusOK, settingsSaveResponse{
		OK:       true,
		Message:  "saved",
		Settings: currentSettings,
	})
}

func (a *app) handleRunNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := a.requireUser(w, r); !ok {
		return
	}

	if err := a.scheduler.execute(r.Context(), time.Time{}); err != nil {
		http.Redirect(w, r, "/?flash="+url.QueryEscape("Manual run failed: "+err.Error()), http.StatusFound)
		return
	}

	http.Redirect(w, r, "/?flash="+url.QueryEscape("Manual update completed."), http.StatusFound)
}

func (a *app) requireUser(w http.ResponseWriter, r *http.Request) (discordUser, bool) {
	if r.URL.Path == "/auth/discord/login" || r.URL.Path == "/auth/discord/callback" {
		return discordUser{}, false
	}
	user, err := a.readSession(r)
	if err != nil {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/auth/discord/login", http.StatusFound)
		} else {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return discordUser{}, false
	}
	return user, true
}

func (a *app) readSession(r *http.Request) (discordUser, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return discordUser{}, err
	}

	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return discordUser{}, errors.New("invalid session")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return discordUser{}, err
	}

	expected := signValue(payload, a.cfg.SessionSecret)
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return discordUser{}, errors.New("invalid signature")
	}

	var user discordUser
	if err := json.Unmarshal(payload, &user); err != nil {
		return discordUser{}, err
	}
	return user, nil
}

func (a *app) writeSession(w http.ResponseWriter, user discordUser) error {
	payload, err := json.Marshal(user)
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signed := signValue(payload, a.cfg.SessionSecret)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    encoded + "." + signed,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(a.cfg.DiscordRedirect),
		MaxAge:   7 * 24 * 3600,
	})
	return nil
}

func signValue(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func discordAvatarURL(user discordUser) string {
	if strings.TrimSpace(user.Avatar) == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=128", user.ID, user.Avatar)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, "failed to encode json response", http.StatusInternalServerError)
	}
}

func (a *app) renderTemplate(w http.ResponseWriter, name string, data any) {
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Printf("render template %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (a *app) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.logger.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func randomHex(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isHTTPS(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && parsed.Scheme == "https"
}

func parseScheduleTime(raw string) (int, int, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Join(strings.Fields(raw), " ")

	for _, layout := range []string{"15:04", "15:04:05", "3:04 PM", "3:04:05 PM"} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.Hour(), parsed.Minute(), nil
		}
	}

	matched := regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?(?:\s?[AP]M)?\b`).FindString(strings.ToUpper(raw))
	if matched != "" {
		for _, layout := range []string{"15:04", "15:04:05", "3:04 PM", "3:04:05 PM"} {
			parsed, err := time.Parse(layout, matched)
			if err == nil {
				return parsed.Hour(), parsed.Minute(), nil
			}
		}
	}

	return 0, 0, errors.New("time must be HH:MM, HH:MM:SS, or include AM/PM")
}

func loadScheduleLocation(timezone string) (*time.Location, error) {
	loc, err := time.LoadLocation(strings.TrimSpace(timezone))
	if err != nil {
		return nil, err
	}
	return loc, nil
}
