package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
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

// ---------------------
// HTTP Public Url
// ---------------------
// buildPublicAssetURL arma una URL pública https para un asset del tenant.
// Espera que el archivo exista en: configs/{tenant}/assets/{path}
// Y que esté expuesto por HTTP en: /tenants/{tenant}/assets/{path}
func buildPublicAssetURL(tenant string, assetPath string) (string, error) {
	base := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/")
	if base == "" {
		return "", fmt.Errorf("PUBLIC_BASE_URL no está configurada")
	}

	assetPath = strings.TrimLeft(assetPath, "/")
	clean := path.Clean(assetPath)

	// Seguridad: evitar traversal (..)
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "../") {
		return "", fmt.Errorf("assetPath inválido: %q", assetPath)
	}

	// Escapar segmentos para URL (por si hay espacios, etc.)
	parts := strings.Split(clean, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	escapedPath := strings.Join(parts, "/")

	return fmt.Sprintf("%s/tenants/%s/assets/%s", base, url.PathEscape(tenant), escapedPath), nil
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
	Type string `json:"type"` // "text" | "interactive_list" | "interactive_buttons"
	Body string `json:"body"`

	// Action: Nombre de la función a ejecutar en Go antes de renderizar (ej: "fetch_client_data", "check_calendar")
	Action string `json:"action,omitempty"`

	// Optional header media for interactive messages (e.g. image header)
	HeaderMedia *FlowHeaderMedia `json:"header_media,omitempty"`

	// List / Buttons UI
	List    *FlowList    `json:"list,omitempty"`
	Buttons *FlowButtons `json:"buttons,omitempty"`

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

type FlowButtons struct {
	Header  string       `json:"header"`
	Footer  string       `json:"footer"`
	Buttons []FlowButton `json:"buttons"`
}

type FlowButton struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type FlowHeaderMedia struct {
	Type string `json:"type"`           // "image" (extendible)
	Path string `json:"path,omitempty"` // local: relative to configs/{tenant}/assets/
	URL  string `json:"url,omitempty"`  // remote: absolute https://...
}

// ---------------------
// Sessions (in-memory)
// ---------------------

type UserSession struct {
	State     string
	UpdatedAt time.Time
	// Agregamos un mapa de datos para guardar info del CRM, selecciones del usuario, etc.
	Data map[string]string
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

		// -------------------------
		// header_media validation (interactive only)
		// -------------------------
		if st.HeaderMedia != nil {
			mt := strings.ToLower(strings.TrimSpace(st.HeaderMedia.Type))
			if mt == "" {
				errs = append(errs, fmt.Sprintf("state=%s header_media.type vacío", stateName))
			} else if mt != "image" {
				errs = append(errs, fmt.Sprintf("state=%s header_media.type no soportado: %q", stateName, st.HeaderMedia.Type))
			}
			if strings.TrimSpace(st.HeaderMedia.URL) == "" && strings.TrimSpace(st.HeaderMedia.Path) == "" {
				errs = append(errs, fmt.Sprintf("state=%s header_media requiere url o path", stateName))
			}
		}

		// -------------------------
		// interactive_list
		// -------------------------
		if st.Type == "interactive_list" {
			if st.List == nil {
				errs = append(errs, fmt.Sprintf("state=%s es interactive_list pero list es nil", stateName))
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
					if strings.TrimSpace(row.ID) == "" {
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

			continue
		}

		// -------------------------
		// interactive_buttons
		// -------------------------
		if st.Type == "interactive_buttons" {
			if st.Buttons == nil {
				errs = append(errs, fmt.Sprintf("state=%s es interactive_buttons pero buttons es nil", stateName))
				continue
			}
			b := st.Buttons

			// Header/Footer: límites similares a list (siempre conviene mantenerlos cortos)
			if runeLen(b.Header) > 60 {
				errs = append(errs, fmt.Sprintf("state=%s buttons.header > 60 (%d): %q", stateName, runeLen(b.Header), b.Header))
			}
			if runeLen(b.Footer) > 60 {
				errs = append(errs, fmt.Sprintf("state=%s buttons.footer > 60 (%d): %q", stateName, runeLen(b.Footer), b.Footer))
			}

			// Botones: 1..3
			if len(b.Buttons) == 0 {
				errs = append(errs, fmt.Sprintf("state=%s no tiene buttons (debe tener 1 a 3)", stateName))
				continue
			}
			if len(b.Buttons) > 3 {
				errs = append(errs, fmt.Sprintf("state=%s tiene %d botones (>3)", stateName, len(b.Buttons)))
			}

			for _, btn := range b.Buttons {
				if strings.TrimSpace(btn.ID) == "" {
					errs = append(errs, fmt.Sprintf("state=%s button id vacío (title=%q)", stateName, btn.Title))
				}
				// Título de botón: recomendación segura <= 20
				if runeLen(btn.Title) > 20 {
					errs = append(errs, fmt.Sprintf("state=%s button title > 20 (%d): %q", stateName, runeLen(btn.Title), btn.Title))
				}
			}

			continue
		}

		// Para otros tipos ("text"), no validamos UI acá.
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

func normalizeRecipientForMeta(to string) string {
	// Normaliza para WhatsApp Cloud API (test) — Argentina:
	// wa_id suele venir como 549XXXXXXXXXX, pero en el "allowed list" / test env
	// muchas veces Meta espera 54XXXXXXXXXX (sin el 9).
	//
	// Importante: solo aplicar fuera de prod (en prod esto puede no ser necesario).
	env := strings.TrimSpace(os.Getenv("APP_ENV"))
	if env == "" {
		env = "dev"
	}
	if env == "prod" {
		return to
	}

	// Meta espera el número sin "+"
	to = strings.TrimSpace(to)
	to = strings.TrimPrefix(to, "+")

	// AR workaround: 549... -> 54...
	if strings.HasPrefix(to, "549") && len(to) > 3 {
		return "54" + to[3:]
	}

	return to
}

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
	to = normalizeRecipientForMeta(to)
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

func (c *WhatsAppClient) sendList(to string, headerText, headerImageURL, body, footer, buttonText string, sections []FlowSection) error {
	toOriginal := to
	if c.forceTo != "" {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s", toOriginal, c.forceTo)
		to = c.forceTo
	}
	to = normalizeRecipientForMeta(to)

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

	if strings.TrimSpace(headerImageURL) != "" {
		interactive["header"] = map[string]any{
			"type": "image",
			"image": map[string]any{
				"link": headerImageURL,
			},
		}
	} else if strings.TrimSpace(headerText) != "" {
		interactive["header"] = map[string]any{
			"type": "text",
			"text": headerText,
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

func (c *WhatsAppClient) sendButtons(to string, headerText, headerImageURL, body, footer string, buttons []FlowButton) error {
	toOriginal := to
	if c.forceTo != "" {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s", toOriginal, c.forceTo)
		to = c.forceTo
	}

	to = normalizeRecipientForMeta(to)

	waButtons := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		waButtons = append(waButtons, map[string]any{
			"type": "reply",
			"reply": map[string]any{
				"id":    b.ID,
				"title": b.Title,
			},
		})
	}

	interactive := map[string]any{
		"type": "button",
		"body": map[string]any{
			"text": body,
		},
		"action": map[string]any{
			"buttons": waButtons,
		},
	}

	if strings.TrimSpace(headerImageURL) != "" {
		interactive["header"] = map[string]any{
			"type": "image",
			"image": map[string]any{
				"link": headerImageURL,
			},
		}
	} else if strings.TrimSpace(headerText) != "" {
		interactive["header"] = map[string]any{
			"type": "text",
			"text": headerText,
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
		headerText := renderVars(st.List.Header, vars)
		footer := renderVars(st.List.Footer, vars)
		button := renderVars(st.List.ButtonText, vars)

		// Optional: header media (image) for interactive messages
		headerImageURL := ""
		if st.HeaderMedia != nil && strings.EqualFold(st.HeaderMedia.Type, "image") {
			if strings.TrimSpace(st.HeaderMedia.URL) != "" {
				headerImageURL = strings.TrimSpace(st.HeaderMedia.URL)
			} else if strings.TrimSpace(st.HeaderMedia.Path) != "" {
				u, err := buildPublicAssetURL(tenant, renderVars(st.HeaderMedia.Path, vars))
				if err != nil {
					return err
				}
				headerImageURL = u
			}
		}

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

		return wa.sendList(to, headerText, headerImageURL, bodyText, footer, button, sections)

	case "interactive_buttons":
		if st.Buttons == nil {
			return fmt.Errorf("estado %s es interactive_buttons pero buttons es nil", stateName)
		}

		bodyText := strings.TrimSpace(st.Body)
		if bodyText == "" {
			bodyText = "Elegí una opción:"
		}
		bodyText = renderVars(bodyText, vars)

		headerText := renderVars(st.Buttons.Header, vars)
		footer := renderVars(st.Buttons.Footer, vars)

		// Optional: header media (image) for interactive messages
		headerImageURL := ""
		if st.HeaderMedia != nil && strings.EqualFold(st.HeaderMedia.Type, "image") {
			if strings.TrimSpace(st.HeaderMedia.URL) != "" {
				headerImageURL = strings.TrimSpace(st.HeaderMedia.URL)
			} else if strings.TrimSpace(st.HeaderMedia.Path) != "" {
				u, err := buildPublicAssetURL(tenant, renderVars(st.HeaderMedia.Path, vars))
				if err != nil {
					return err
				}
				headerImageURL = u
			}
		}

		btns := make([]FlowButton, 0, len(st.Buttons.Buttons))
		for _, b := range st.Buttons.Buttons {
			btns = append(btns, FlowButton{
				ID:    b.ID,
				Title: renderVars(b.Title, vars),
			})
		}

		return wa.sendButtons(to, headerText, headerImageURL, bodyText, footer, btns)

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

				// Inicializamos vars con datos básicos
				vars := map[string]string{
					"name": name,
				}

				sessKey := tenant + ":" + waID
				sess, ok := a.sessions.Get(sessKey)
				// Si no existe sesión o no tiene estado, inicializamos
				if !ok || sess.State == "" {
					sess = UserSession{
						State:     "MENU",
						UpdatedAt: time.Now(),
						Data:      make(map[string]string), // Importante inicializar el mapa
					}
					a.sessions.Set(sessKey, sess)
				}

				// Si la sesión ya traía datos (Data), los sumamos a vars para que estén disponibles
				if sess.Data != nil {
					for k, v := range sess.Data {
						vars[k] = v
					}
				}

				log.Printf("🤖 tenant=%s wa_id=%s state=%s type=%s name=%s", tenant, waID, sess.State, msg.Type, name)

				waClient, err := NewWhatsAppClient(phoneID)
				if err != nil {
					log.Printf("ERROR WhatsApp client: %v", err)
					continue
				}

				// ---------------------------------------------------------
				// NUEVO BLOQUE: CAPTURAR SELECCIÓN INTERACTIVA (SLOTS)
				// ---------------------------------------------------------
				// Si el mensaje es una respuesta a botón o lista, guardamos el ID
				// en la sesión ANTES de calcular el próximo estado.
				if msg.Type == "interactive" && msg.Interactive != nil {
					selectedID := ""
					if msg.Interactive.ListReply != nil {
						selectedID = msg.Interactive.ListReply.ID
					} else if msg.Interactive.ButtonReply != nil {
						selectedID = msg.Interactive.ButtonReply.ID
					}

					if selectedID != "" {
						if sess.Data == nil {
							sess.Data = make(map[string]string)
						}
						sess.Data["last_selected_id"] = selectedID
						log.Printf("💾 Guardando selección del usuario: %s", selectedID)
					}
				}
				// ---------------------------------------------------------

				// 1. Determinamos el siguiente estado según el input del usuario
				nextState, handled, err := a.processMessage(tenant, sess.State, msg)
				if err != nil {
					log.Printf("ERROR procesando msg: %v", err)
					_ = waClient.sendText(waID, "Perdón, hubo un error. Probá de nuevo.")
					continue
				}

				if !handled {
					nextState = "MENU"
				}

				// ---------------------------------------------------------
				// NUEVA LÓGICA: EJECUCIÓN DE ACCIONES (The Action Pattern)
				// ---------------------------------------------------------

				// Recuperamos la config para ver si el nextState tiene una Action asociada
				cfg, ok := a.cache.Get(tenant)
				if !ok {
					// Si por alguna razón no está en caché (raro), intentamos recargar
					loaded, errLoad := loadFlowConfig(tenant)
					if errLoad == nil {
						cfg = loaded
						a.cache.Set(tenant, loaded)
					}
				}

				// Buscamos si el próximo estado tiene una acción definida
				targetSt, exists := cfg.States[nextState]

				// Si el estado existe y tiene una Action definida...
				if exists && targetSt.Action != "" {
					log.Printf("⚡ Ejecutando acción: %s [Estado: %s]", targetSt.Action, nextState)

					// Buscamos la función en el registro
					if fn, found := actionRegistry[targetSt.Action]; found {
						// Ejecutamos la acción pasándole el contexto
						newVars, errAction := fn(tenant, waID, &sess)

						if errAction != nil {
							log.Printf("❌ Error ejecutando acción %s: %v", targetSt.Action, errAction)
							// Opcional: Podrías forzar nextState = "ERROR_STATE" aquí si quisieras
						} else {
							// Merge de variables nuevas
							if sess.Data == nil {
								sess.Data = make(map[string]string)
							}
							for k, v := range newVars {
								// 1. Disponibles para el render inmediato
								vars[k] = v
								// 2. Persistentes en la sesión del usuario
								sess.Data[k] = v
							}
						}
					} else {
						log.Printf("⚠️ Acción definida en JSON pero no en código: %s", targetSt.Action)
					}
				}

				// ---------------------------------------------------------

				// Guardamos la sesión actualizada (Nuevo Estado + Nuevos Datos en Data)
				sess.State = nextState
				sess.UpdatedAt = time.Now()
				a.sessions.Set(sessKey, sess)

				// Renderizamos y enviamos el mensaje
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

func actionScheduleAppointment(tenant, userID string, sess *UserSession) (map[string]string, error) {
	// 1. Recuperamos qué botón apretó el usuario (lo guardamos recién en handleMessage)
	selectedID := sess.Data["last_selected_id"] // Ej: "SLOT_1"

	// 2. Recuperamos el valor ISO oculto asociado a ese botón (lo guardó get_calendar_slots)
	// Ej: si elegiste SLOT_1, buscamos SLOT_1_ISO
	isoDate := sess.Data[selectedID+"_ISO"]

	if isoDate == "" {
		log.Printf("❌ No se encontró fecha para el ID: %s. Datos en sesión: %v", selectedID, sess.Data)
		return nil, fmt.Errorf("no seleccionaste un horario válido o expiró la sesión")
	}

	// 3. Instanciamos el servicio de calendario
	svc, err := NewCalendarService(tenant)
	if err != nil {
		return nil, err
	}

	// 4. Datos del paciente
	name := sess.Data["name"]
	if clientName, ok := sess.Data["client_name"]; ok && clientName != "" {
		name = clientName
	}

	log.Printf("📅 Agendando turno real en Google para %s en %s", name, isoDate)

	// 5. Llamamos a Google Calendar
	err = svc.CreateAppointment(isoDate, name, userID) // userID es el teléfono
	if err != nil {
		log.Printf("❌ Error creando evento en Google: %v", err)
		return nil, fmt.Errorf("error al agendar en Google")
	}

	// Devolvemos variables para mostrar en el mensaje de confirmación
	return map[string]string{
		"appointment_confirm_time": isoDate,
	}, nil
}

// ---------------------
// Tenant assets (served from /configs/{tenant}/assets/* via public route)
// ---------------------

func (a *App) handleTenantAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// URL: /tenants/{tenant}/assets/{path}
	path := strings.TrimPrefix(r.URL.Path, "/tenants/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	tenant := parts[0]
	if parts[1] != "assets" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rel := parts[2]
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, string(filepath.Separator)+".."+string(filepath.Separator)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	baseDir := filepath.Join(configRoot, tenant, "assets")
	filePath := filepath.Join(baseDir, clean)

	// Prevent path traversal
	absBase, err1 := filepath.Abs(baseDir)
	absFile, err2 := filepath.Abs(filePath)
	if err1 != nil || err2 != nil || !strings.HasPrefix(absFile, absBase+string(filepath.Separator)) && absFile != absBase {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Set content type if possible
	if ct := mime.TypeByExtension(filepath.Ext(absFile)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, absFile)
}

// ActionFunc define la firma de nuestras acciones.
// Recibe el tenant, el ID del usuario, y la sesión actual.
// Devuelve un mapa de variables nuevas para inyectar en el template o un error.
type ActionFunc func(tenant, userID string, session *UserSession) (map[string]string, error)

var actionRegistry = map[string]ActionFunc{
	"mock_crm_lookup":      actionMockCRMLookup,
	"get_calendar_slots":   actionGetCalendarSlots,
	"schedule_appointment": actionScheduleAppointment,
}

// --- Implementación Mock del CRM ---

func actionMockCRMLookup(tenant, userID string, sess *UserSession) (map[string]string, error) {
	// SIMULAMOS una llamada a base de datos
	// En la vida real, acá harías: SELECT * FROM users WHERE phone = userID

	log.Printf("🔍 Buscando usuario %s en CRM simulado...", userID)

	// Simulamos que si el número termina en par, es cliente. Si es impar, es nuevo.
	// (Un hack rápido para probar flujos distintos con distintos celulares)
	esCliente := false
	if len(userID) > 0 {
		lastDigit := userID[len(userID)-1]
		if int(lastDigit)%2 == 0 {
			esCliente = true
		}
	}

	vars := make(map[string]string)
	if esCliente {
		vars["is_client"] = "true"
		vars["client_name"] = "Carlos (Cliente VIP)" // Dato traído del "CRM"
		vars["last_visit"] = "15 de Febrero"
	} else {
		vars["is_client"] = "false"
		vars["client_name"] = "Visitante"
	}

	return vars, nil
}

func actionGetCalendarSlots(tenant, userID string, sess *UserSession) (map[string]string, error) {
	log.Println("📅 Consultando Google Calendar real...")

	// 1. Instanciamos el servicio (busca calendar.json del tenant)
	svc, err := NewCalendarService(tenant)
	if err != nil {
		log.Printf("ERROR Calendar Init: %v", err)
		return map[string]string{"slot_1": "Error Config"}, nil
	}

	// 2. Pedimos los slots libres a Google
	slots, err := svc.GetNextAvailableSlots()
	if err != nil {
		log.Printf("ERROR Calendar Query: %v", err)
		return map[string]string{"slot_1": "Sin sistema"}, nil
	}

	vars := make(map[string]string)

	// Limpiamos variables viejas para que no queden botones rotos
	vars["slot_1"] = "Sin cupo"
	vars["slot_2"] = "-"
	vars["slot_3"] = "-"

	// 3. Rellenamos las variables
	for i, s := range slots {
		// Variable visible en el botón (ej: "Lun 18 10:00")
		keyText := fmt.Sprintf("slot_%d", i+1)
		vars[keyText] = s.Text

		// Variable OCULTA con la fecha real (ej: "2026-02-18T10:00:00Z")
		// Esta es la que usa schedule_appointment
		vars[fmt.Sprintf("%s_ISO", s.ID)] = s.ISOValue
	}

	return vars, nil
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
	http.HandleFunc("/tenants/", app.handleTenantAssets)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("Webhook escuchando en %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
