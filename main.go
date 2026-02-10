package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"
)

const (
	apiVersion = "v24.0"
	configRoot = "configs"
)

/*
ENV:

VERIFY_TOKEN=brokerbot_verify
WHATSAPP_TOKEN=EAAM...

# Mapeo tenant (por phone_number_id)
TENANT_BY_PHONE_NUMBER_ID=1041740029016016:broker
DEFAULT_TENANT=broker

# SOLO PARA DEV/PRUEBAS: fuerza a quién le respondés
WHATSAPP_FORCE_TO=+54111558492828

# Ambiente y puerto
APP_ENV=dev
PORT=8080
*/

// ---------------------
// Env loader
// ---------------------

func loadEnvFiles() {
	env := strings.TrimSpace(os.Getenv("APP_ENV"))
	if env == "" {
		env = "dev"
	}

	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env." + env)

	finalEnv := os.Getenv("APP_ENV")
	if finalEnv == "" {
		finalEnv = env
	}
	log.Printf("🔧 APP_ENV=%s (cargado .env y .env.%s si existen)", finalEnv, env)
}

// ---------------------
// Simple templating: {{name}}
// ---------------------

func renderVars(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

type WebhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []IncomingMessage `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type IncomingMessage struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`

	Text *struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`

	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply,omitempty"`
		ListReply *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"list_reply,omitempty"`
	} `json:"interactive,omitempty"`
}

// ---------------------
// Flow config (List)
// ---------------------

type FlowConfig struct {
	Version string               `json:"version"`
	States  map[string]FlowState `json:"states"`
}

type FlowState struct {
	Type string `json:"type"` // "text" | "interactive_list"
	Body string `json:"body"`

	// List UI
	List *FlowList `json:"list,omitempty"`

	// Transiciones
	OnTextNext   string            `json:"on_text_next,omitempty"`
	OnSelectNext map[string]string `json:"on_select_next,omitempty"` // row_id -> next_state
}

type FlowList struct {
	Header     string        `json:"header"`
	ButtonText string        `json:"button_text"`
	Footer     string        `json:"footer"`
	Sections   []FlowSection `json:"sections"`
}

type FlowSection struct {
	Title string    `json:"title"`
	Rows  []FlowRow `json:"rows"`
}

type FlowRow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// ---------------------
// Sessions (in-memory)
// ---------------------

type UserSession struct {
	State     string
	UpdatedAt time.Time
}

type SessionStore struct {
	mu   sync.RWMutex
	data map[string]UserSession
}

func NewSessionStore() *SessionStore {
	return &SessionStore{data: make(map[string]UserSession)}
}

func (s *SessionStore) Get(key string) (UserSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *SessionStore) Set(key string, sess UserSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = sess
}

// ---------------------
// Config cache
// ---------------------

type ConfigCache struct {
	mu    sync.RWMutex
	cache map[string]FlowConfig
}

func NewConfigCache() *ConfigCache {
	return &ConfigCache{cache: make(map[string]FlowConfig)}
}

func (c *ConfigCache) Get(tenant string) (FlowConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cfg, ok := c.cache[tenant]
	return cfg, ok
}

func (c *ConfigCache) Set(tenant string, cfg FlowConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[tenant] = cfg
}

func loadFlowConfig(tenant string) (FlowConfig, error) {
	path := filepath.Join(configRoot, tenant, "flow.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return FlowConfig{}, fmt.Errorf("no pude leer %s: %w", path, err)
	}
	var cfg FlowConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return FlowConfig{}, fmt.Errorf("json inválido en %s: %w", path, err)
	}
	if len(cfg.States) == 0 {
		return FlowConfig{}, fmt.Errorf("flow.json de %s no tiene states", tenant)
	}
	if err := validateFlowConfig(tenant, cfg); err != nil {
		return FlowConfig{}, err
	}
	return cfg, nil
}

// ---------------------
// Flow validation (WhatsApp limits)
// ---------------------

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func validateFlowConfig(tenant string, cfg FlowConfig) error {
	var errs []string

	for stateName, st := range cfg.States {
		if st.Type != "interactive_list" || st.List == nil {
			continue
		}
		l := st.List

		if runeLen(l.Header) > 60 {
			errs = append(errs, fmt.Sprintf("state=%s header > 60 (%d): %q", stateName, runeLen(l.Header), l.Header))
		}
		if runeLen(l.Footer) > 60 {
			errs = append(errs, fmt.Sprintf("state=%s footer > 60 (%d): %q", stateName, runeLen(l.Footer), l.Footer))
		}
		if runeLen(l.ButtonText) > 20 {
			errs = append(errs, fmt.Sprintf("state=%s button_text > 20 (%d): %q", stateName, runeLen(l.ButtonText), l.ButtonText))
		}

		for _, sec := range l.Sections {
			if runeLen(sec.Title) > 24 {
				errs = append(errs, fmt.Sprintf("state=%s section title > 24 (%d): %q", stateName, runeLen(sec.Title), sec.Title))
			}
			for _, row := range sec.Rows {
				if row.ID == "" {
					errs = append(errs, fmt.Sprintf("state=%s row id vacío (title=%q)", stateName, row.Title))
				}
				if runeLen(row.Title) > 24 {
					errs = append(errs, fmt.Sprintf("state=%s row title > 24 (%d): %q", stateName, runeLen(row.Title), row.Title))
				}
				if runeLen(row.Description) > 72 {
					errs = append(errs, fmt.Sprintf("state=%s row desc > 72 (%d): %q", stateName, runeLen(row.Description), row.Description))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("flow inválido tenant=%s:\n- %s", tenant, strings.Join(errs, "\n- "))
	}
	return nil
}

// ---------------------
// Tenant resolver
// ---------------------

type TenantResolver struct {
	byPhoneNumberID map[string]string
	defaultTenant   string
}

func NewTenantResolver() *TenantResolver {
	m := map[string]string{}
	raw := os.Getenv("TENANT_BY_PHONE_NUMBER_ID")
	if raw != "" {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			kv := strings.SplitN(p, ":", 2)
			if len(kv) != 2 {
				continue
			}
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	def := os.Getenv("DEFAULT_TENANT")
	if def == "" {
		def = "broker"
	}
	return &TenantResolver{byPhoneNumberID: m, defaultTenant: def}
}

func (r *TenantResolver) Resolve(phoneNumberID string) string {
	if t, ok := r.byPhoneNumberID[phoneNumberID]; ok && t != "" {
		return t
	}
	return r.defaultTenant
}

// ---------------------
// WhatsApp client (Cloud API)
// ---------------------

type WhatsAppClient struct {
	token      string
	phoneID    string
	apiBaseURL string
	forceTo    string
}

func NewWhatsAppClient(phoneNumberID string) (*WhatsAppClient, error) {
	token := os.Getenv("WHATSAPP_TOKEN")
	if token == "" {
		return nil, errors.New("WHATSAPP_TOKEN no seteado")
	}

	env := strings.TrimSpace(os.Getenv("APP_ENV"))
	if env == "" {
		env = "dev"
	}
	force := os.Getenv("WHATSAPP_FORCE_TO")
	if env != "dev" {
		force = ""
	}

	return &WhatsAppClient{
		token:      token,
		phoneID:    phoneNumberID,
		apiBaseURL: fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", apiVersion, phoneNumberID),
		forceTo:    force,
	}, nil
}

func (c *WhatsAppClient) sendText(to string, body string) error {
	toOriginal := to
	if c.forceTo != "" {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s", toOriginal, c.forceTo)
		to = c.forceTo
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text": map[string]any{
			"body": body,
		},
	}
	return c.post(payload)
}

func (c *WhatsAppClient) sendList(to string, header, body, footer, buttonText string, sections []FlowSection) error {
	toOriginal := to
	if c.forceTo != "" {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s", toOriginal, c.forceTo)
		to = c.forceTo
	}

	waSections := make([]map[string]any, 0, len(sections))
	for _, s := range sections {
		rows := make([]map[string]any, 0, len(s.Rows))
		for _, r := range s.Rows {
			row := map[string]any{
				"id":    r.ID,
				"title": r.Title,
			}
			if strings.TrimSpace(r.Description) != "" {
				row["description"] = r.Description
			}
			rows = append(rows, row)
		}
		sec := map[string]any{
			"title": s.Title,
			"rows":  rows,
		}
		waSections = append(waSections, sec)
	}

	interactive := map[string]any{
		"type": "list",
		"body": map[string]any{
			"text": body,
		},
		"action": map[string]any{
			"button":   buttonText,
			"sections": waSections,
		},
	}

	if strings.TrimSpace(header) != "" {
		interactive["header"] = map[string]any{
			"type": "text",
			"text": header,
		}
	}

	if strings.TrimSpace(footer) != "" {
		interactive["footer"] = map[string]any{
			"text": footer,
		}
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "interactive",
		"interactive":       interactive,
	}

	return c.post(payload)
}

func (c *WhatsAppClient) post(payload map[string]any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.apiBaseURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("respuesta no OK de Meta: %s - %s", resp.Status, string(body))
	}
	log.Printf("✅ Enviado OK: %s", string(body))
	return nil
}

// ---------------------
// Renderer
// ---------------------

type Renderer struct {
	cache *ConfigCache
}

func NewRenderer(cache *ConfigCache) *Renderer {
	return &Renderer{cache: cache}
}

func (r *Renderer) RenderAndSend(tenant string, stateName string, wa *WhatsAppClient, to string, vars map[string]string) error {
	cfg, ok := r.cache.Get(tenant)
	if !ok {
		loaded, err := loadFlowConfig(tenant)
		if err != nil {
			return err
		}
		r.cache.Set(tenant, loaded)
		cfg = loaded
	}

	st, ok := cfg.States[stateName]
	if !ok {
		return fmt.Errorf("estado no existe: %s", stateName)
	}

	switch st.Type {
	case "text":
		return wa.sendText(to, renderVars(st.Body, vars))

	case "interactive_list":
		if st.List == nil {
			return fmt.Errorf("estado %s es interactive_list pero list es nil", stateName)
		}

		// ✅ Un solo mensaje: el body del interactive es st.Body (no mandamos texto aparte)
		bodyText := strings.TrimSpace(st.Body)
		if bodyText == "" {
			bodyText = "Elegí una opción:"
		}
		bodyText = renderVars(bodyText, vars)

		// Render vars también en UI del list
		header := renderVars(st.List.Header, vars)
		footer := renderVars(st.List.Footer, vars)
		button := renderVars(st.List.ButtonText, vars)

		// Render vars en secciones/rows (por si lo necesitás)
		sections := make([]FlowSection, 0, len(st.List.Sections))
		for _, s := range st.List.Sections {
			ns := FlowSection{
				Title: renderVars(s.Title, vars),
				Rows:  make([]FlowRow, 0, len(s.Rows)),
			}
			for _, row := range s.Rows {
				ns.Rows = append(ns.Rows, FlowRow{
					ID:          row.ID,
					Title:       renderVars(row.Title, vars),
					Description: renderVars(row.Description, vars),
				})
			}
			sections = append(sections, ns)
		}

		return wa.sendList(to, header, bodyText, footer, button, sections)

	default:
		return fmt.Errorf("tipo de estado no soportado: %s", st.Type)
	}
}

// ---------------------
// App (handler)
// ---------------------

type App struct {
	verifyToken string
	resolver    *TenantResolver
	sessions    *SessionStore
	cache       *ConfigCache
	renderer    *Renderer
}

func NewApp() (*App, error) {
	verify := os.Getenv("VERIFY_TOKEN")
	if verify == "" {
		verify = "brokerbot_verify"
	}
	cache := NewConfigCache()
	return &App{
		verifyToken: verify,
		resolver:    NewTenantResolver(),
		sessions:    NewSessionStore(),
		cache:       cache,
		renderer:    NewRenderer(cache),
	}, nil
}

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		a.handleVerify(w, r)
		return
	case "POST":
		a.handleMessage(w, r)
		return
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
}

func (a *App) handleVerify(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	if mode == "subscribe" && token == a.verifyToken {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(challenge))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

func (a *App) handleMessage(w http.ResponseWriter, r *http.Request) {
	log.Printf(">> POST /webhook from %s", r.RemoteAddr)

	log.Printf("POST headers=%v", r.Header)
	rawBody, _ := io.ReadAll(r.Body)
	log.Printf("POST body=%s", string(rawBody))

	var payload WebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		log.Printf("ERROR unmarshal: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, e := range payload.Entry {
		for _, ch := range e.Changes {
			phoneID := ch.Value.Metadata.PhoneNumberID
			tenant := a.resolver.Resolve(phoneID)

			if len(ch.Value.Messages) == 0 {
				continue
			}

			for _, msg := range ch.Value.Messages {
				waID := msg.From
				name := ""
				if len(ch.Value.Contacts) > 0 {
					name = strings.TrimSpace(ch.Value.Contacts[0].Profile.Name)
				}
				if name == "" {
					name = "ahí"
				}

				vars := map[string]string{
					"name": name,
				}

				sessKey := tenant + ":" + waID
				sess, ok := a.sessions.Get(sessKey)
				if !ok || sess.State == "" {
					sess = UserSession{State: "MENU", UpdatedAt: time.Now()}
					a.sessions.Set(sessKey, sess)
				}

				log.Printf("🤖 tenant=%s wa_id=%s state=%s type=%s name=%s", tenant, waID, sess.State, msg.Type, name)

				waClient, err := NewWhatsAppClient(phoneID)
				if err != nil {
					log.Printf("ERROR WhatsApp client: %v", err)
					continue
				}

				nextState, handled, err := a.processMessage(tenant, sess.State, msg)
				if err != nil {
					log.Printf("ERROR procesando msg: %v", err)
					_ = waClient.sendText(waID, "Perdón, hubo un error. Probá de nuevo.")
					continue
				}

				if !handled {
					nextState = "MENU"
				}

				a.sessions.Set(sessKey, UserSession{State: nextState, UpdatedAt: time.Now()})

				if err := a.renderer.RenderAndSend(tenant, nextState, waClient, waID, vars); err != nil {
					log.Printf("ERROR render %s: %v", nextState, err)
					_ = waClient.sendText(waID, "Perdón, hubo un problema mostrando el menú.")
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (a *App) processMessage(tenant string, state string, msg IncomingMessage) (next string, handled bool, err error) {
	cfg, ok := a.cache.Get(tenant)
	if !ok {
		loaded, err2 := loadFlowConfig(tenant)
		if err2 != nil {
			return "", false, err2
		}
		a.cache.Set(tenant, loaded)
		cfg = loaded
	}

	st, ok := cfg.States[state]
	if !ok {
		return "MENU", false, nil
	}

	switch msg.Type {
	case "text":
		if msg.Text == nil {
			return "MENU", false, nil
		}
		txt := strings.TrimSpace(msg.Text.Body)
		log.Printf("📩 TEXT: %q", txt)

		if strings.EqualFold(txt, "menu") {
			return "MENU", true, nil
		}

		if st.OnTextNext != "" {
			return st.OnTextNext, true, nil
		}
		return "MENU", false, nil

	case "interactive":
		if msg.Interactive == nil {
			return "MENU", false, nil
		}

		switch msg.Interactive.Type {
		case "list_reply":
			if msg.Interactive.ListReply == nil {
				return "MENU", false, nil
			}
			rowID := msg.Interactive.ListReply.ID
			log.Printf("🧾 LIST_REPLY: id=%s title=%s", rowID, msg.Interactive.ListReply.Title)

			if st.OnSelectNext != nil {
				if ns, ok := st.OnSelectNext[rowID]; ok && ns != "" {
					return ns, true, nil
				}
			}
			return "MENU", false, nil

		case "button_reply":
			if msg.Interactive.ButtonReply == nil {
				return "MENU", false, nil
			}
			btnID := msg.Interactive.ButtonReply.ID
			log.Printf("🔘 BUTTON_REPLY: id=%s title=%s", btnID, msg.Interactive.ButtonReply.Title)

			if st.OnSelectNext != nil {
				if ns, ok := st.OnSelectNext[btnID]; ok && ns != "" {
					return ns, true, nil
				}
			}
			return "MENU", false, nil

		default:
			return "MENU", false, nil
		}

	default:
		return "MENU", false, nil
	}
}

// ---------------------
// main
// ---------------------

func main() {
	loadEnvFiles()

	app, err := NewApp()
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/webhook", app.handleWebhook)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("Webhook escuchando en %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
