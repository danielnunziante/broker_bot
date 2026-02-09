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
$env:TENANT_BY_PHONE_NUMBER_ID="1041740029016016:broker"
$env:DEFAULT_TENANT="broker"

# SOLO PARA DEV/PRUEBAS: fuerza a quién le respondés
# (Meta a veces te rompe por whitelist/format)
$env:WHATSAPP_FORCE_TO="+54111558492828"
*/

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
	return cfg, nil
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

func (tr *TenantResolver) Resolve(phoneNumberID string) string {
	if t, ok := tr.byPhoneNumberID[phoneNumberID]; ok {
		return t
	}
	return tr.defaultTenant
}

// ---------------------
// WhatsApp helpers / senders
// ---------------------

func normalizeTo(to string) string {
	to = strings.TrimSpace(to)
	to = strings.ReplaceAll(to, " ", "")
	to = strings.ReplaceAll(to, "-", "")
	return to
}

// Dev-only override:
// Si WHATSAPP_FORCE_TO está seteado, se usa SIEMPRE como destinatario.
// Esto te salva del quilombo del whitelist/formato durante pruebas.
func applyForceTo(to string) (finalTo string, forced bool) {
	force := strings.TrimSpace(os.Getenv("WHATSAPP_FORCE_TO"))
	if force == "" {
		return normalizeTo(to), false
	}
	return normalizeTo(force), true
}

func doMetaPOST(url, token string, payload map[string]any) error {
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error enviando mensaje: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("respuesta no OK de Meta: %s - %s", resp.Status, string(respBody))
	}

	log.Printf("✅ Enviado OK: %s\n", string(respBody))
	return nil
}

func sendWhatsAppText(phoneNumberID, to, text string) error {
	token := os.Getenv("WHATSAPP_TOKEN")
	if token == "" {
		return fmt.Errorf("WHATSAPP_TOKEN no está seteado")
	}

	finalTo, forced := applyForceTo(to)
	if forced {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s\n", normalizeTo(to), finalTo)
	}

	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", apiVersion, phoneNumberID)

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                finalTo,
		"type":              "text",
		"text": map[string]any{
			"body": text,
		},
	}
	return doMetaPOST(url, token, payload)
}

func sendWhatsAppList(phoneNumberID, to, body string, list *FlowList) error {
	token := os.Getenv("WHATSAPP_TOKEN")
	if token == "" {
		return fmt.Errorf("WHATSAPP_TOKEN no está seteado")
	}
	if list == nil {
		return errors.New("list es nil")
	}
	if len(list.Sections) == 0 {
		return errors.New("list no tiene sections")
	}

	finalTo, forced := applyForceTo(to)
	if forced {
		log.Printf("⚠️ WHATSAPP_FORCE_TO activo: to_original=%s to_forzado=%s\n", normalizeTo(to), finalTo)
	}

	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", apiVersion, phoneNumberID)

	// Convertimos sections/rows al formato WhatsApp
	var sections []map[string]any
	for _, s := range list.Sections {
		var rows []map[string]any
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
		sections = append(sections, map[string]any{
			"title": s.Title,
			"rows":  rows,
		})
	}

	interactive := map[string]any{
		"type": "list",
		"body": map[string]any{
			"text": body,
		},
		"action": map[string]any{
			"button":   list.ButtonText,
			"sections": sections,
		},
	}

	if strings.TrimSpace(list.Header) != "" {
		interactive["header"] = map[string]any{
			"type": "text",
			"text": list.Header,
		}
	}
	if strings.TrimSpace(list.Footer) != "" {
		interactive["footer"] = map[string]any{
			"text": list.Footer,
		}
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                finalTo,
		"type":              "interactive",
		"interactive":       interactive,
	}

	return doMetaPOST(url, token, payload)
}

// ---------------------
// Bot engine
// ---------------------

type Bot struct {
	sessions *SessionStore
	configs  *ConfigCache
	tenants  *TenantResolver
}

func NewBot() *Bot {
	return &Bot{
		sessions: NewSessionStore(),
		configs:  NewConfigCache(),
		tenants:  NewTenantResolver(),
	}
}

func (b *Bot) getConfig(tenant string) (FlowConfig, error) {
	if cfg, ok := b.configs.Get(tenant); ok {
		return cfg, nil
	}
	cfg, err := loadFlowConfig(tenant)
	if err != nil {
		return FlowConfig{}, err
	}
	b.configs.Set(tenant, cfg)
	return cfg, nil
}

func (b *Bot) sessionKey(tenant, waID string) string {
	return tenant + "::" + waID
}

func (b *Bot) getState(tenant, waID string) string {
	key := b.sessionKey(tenant, waID)
	if s, ok := b.sessions.Get(key); ok && s.State != "" {
		return s.State
	}
	return "MENU"
}

func (b *Bot) setState(tenant, waID, state string) {
	key := b.sessionKey(tenant, waID)
	b.sessions.Set(key, UserSession{State: state, UpdatedAt: time.Now()})
}

func applyPlaceholders(s, name, lastText string) string {
	s = strings.ReplaceAll(s, "{{name}}", name)
	s = strings.ReplaceAll(s, "{{last_text}}", lastText)
	return s
}

func (b *Bot) render(cfg FlowConfig, phoneNumberID, to, state, name, lastText string) error {
	st, ok := cfg.States[state]
	if !ok {
		return fmt.Errorf("estado %s no existe", state)
	}

	body := applyPlaceholders(st.Body, name, lastText)

	switch st.Type {
	case "text":
		return sendWhatsAppText(phoneNumberID, to, body)

	case "interactive_list":
		if st.List == nil {
			return fmt.Errorf("state %s es list pero list=nil", state)
		}

		// placeholders en header/footer también
		listCopy := *st.List
		listCopy.Header = applyPlaceholders(listCopy.Header, name, lastText)
		listCopy.Footer = applyPlaceholders(listCopy.Footer, name, lastText)
		listCopy.ButtonText = applyPlaceholders(listCopy.ButtonText, name, lastText)

		return sendWhatsAppList(phoneNumberID, to, body, &listCopy)

	default:
		return fmt.Errorf("tipo no soportado: %s", st.Type)
	}
}

func (b *Bot) handle(phoneNumberID string, msg IncomingMessage, displayName string) {
	tenant := b.tenants.Resolve(phoneNumberID)

	cfg, err := b.getConfig(tenant)
	if err != nil {
		log.Printf("ERROR cargando config tenant=%s: %v\n", tenant, err)
		return
	}

	waID := msg.From
	state := b.getState(tenant, waID)

	log.Printf("🤖 tenant=%s wa_id=%s state=%s type=%s name=%s\n", tenant, waID, state, msg.Type, displayName)

	cur, ok := cfg.States[state]
	if !ok {
		state = "MENU"
		cur = cfg.States[state]
		b.setState(tenant, waID, state)
	}

	switch msg.Type {
	case "text":
		userText := ""
		if msg.Text != nil {
			userText = strings.TrimSpace(msg.Text.Body)
		}
		log.Printf("📩 TEXT: %q\n", userText)

		// Si el state actual define transición por texto, vamos ahí
		if cur.OnTextNext != "" {
			next := cur.OnTextNext
			b.setState(tenant, waID, next)
			if err := b.render(cfg, phoneNumberID, waID, next, displayName, userText); err != nil {
				log.Printf("ERROR render: %v\n", err)
			}
			return
		}

		// si el user mete texto en el menú, lo re-mostramos
		if state == "MENU" {
			if err := sendWhatsAppText(phoneNumberID, waID, "Elegí una opción de la lista 👇"); err != nil {
				log.Printf("ERROR send text: %v\n", err)
			}
			if err := b.render(cfg, phoneNumberID, waID, "MENU", displayName, userText); err != nil {
				log.Printf("ERROR render MENU: %v\n", err)
			}
			return
		}

		if err := sendWhatsAppText(phoneNumberID, waID, "No te entendí 😅. Volvamos al menú."); err != nil {
			log.Printf("ERROR send text: %v\n", err)
		}
		b.setState(tenant, waID, "MENU")
		if err := b.render(cfg, phoneNumberID, waID, "MENU", displayName, userText); err != nil {
			log.Printf("ERROR render MENU: %v\n", err)
		}
		return

	case "interactive":
		selectedID := ""
		if msg.Interactive != nil && msg.Interactive.ListReply != nil {
			selectedID = msg.Interactive.ListReply.ID
		}
		log.Printf("🟩 LIST SELECT id=%s\n", selectedID)

		if selectedID == "" {
			if err := sendWhatsAppText(phoneNumberID, waID, "Eso no me llegó bien 😬. Probá de nuevo."); err != nil {
				log.Printf("ERROR send text: %v\n", err)
			}
			if err := b.render(cfg, phoneNumberID, waID, "MENU", displayName, ""); err != nil {
				log.Printf("ERROR render MENU: %v\n", err)
			}
			return
		}

		next, ok := cur.OnSelectNext[selectedID]
		if !ok || next == "" {
			if err := sendWhatsAppText(phoneNumberID, waID, "Esa opción no existe (todavía) 😅. Volvemos al menú."); err != nil {
				log.Printf("ERROR send text: %v\n", err)
			}
			b.setState(tenant, waID, "MENU")
			if err := b.render(cfg, phoneNumberID, waID, "MENU", displayName, ""); err != nil {
				log.Printf("ERROR render MENU: %v\n", err)
			}
			return
		}

		b.setState(tenant, waID, next)
		if err := b.render(cfg, phoneNumberID, waID, next, displayName, ""); err != nil {
			log.Printf("ERROR render: %v\n", err)
		}
		return

	default:
		log.Printf("Mensaje no soportado: type=%s\n", msg.Type)
		if err := sendWhatsAppText(phoneNumberID, waID, "Por ahora solo entiendo texto y lista 🙏"); err != nil {
			log.Printf("ERROR send text: %v\n", err)
		}
		if err := b.render(cfg, phoneNumberID, waID, "MENU", displayName, ""); err != nil {
			log.Printf("ERROR render MENU: %v\n", err)
		}
		return
	}
}

// ---------------------
// HTTP server
// ---------------------

type Server struct {
	bot *Bot
}

func NewServer() *Server {
	return &Server{bot: NewBot()}
}

func (s *Server) webhookHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf(">> %s %s from %s\n", r.Method, r.URL.String(), r.RemoteAddr)

	// GET verify
	if r.Method == http.MethodGet {
		verifyToken := r.URL.Query().Get("hub.verify_token")
		challenge := r.URL.Query().Get("hub.challenge")

		if verifyToken == os.Getenv("VERIFY_TOKEN") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, challenge)
			return
		}

		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "forbidden")
		return
	}

	// POST events
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("ERROR leyendo body: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "bad request")
		return
	}
	defer r.Body.Close()

	log.Printf("POST headers=%v\n", r.Header)
	log.Printf("POST body=%s\n", string(body))

	// respond fast
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")

	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("ERROR parseando JSON: %v\n", err)
		return
	}

	if len(payload.Entry) == 0 || len(payload.Entry[0].Changes) == 0 {
		return
	}

	val := payload.Entry[0].Changes[0].Value
	phoneNumberID := val.Metadata.PhoneNumberID
	if phoneNumberID == "" || len(val.Messages) == 0 {
		return
	}

	displayName := "che"
	if len(val.Contacts) > 0 && val.Contacts[0].Profile.Name != "" {
		displayName = val.Contacts[0].Profile.Name
	}

	msg := val.Messages[0]
	s.bot.handle(phoneNumberID, msg, displayName)
}

func main() {
	srv := NewServer()
	http.HandleFunc("/webhook", srv.webhookHandler)

	log.Println("Webhook escuchando en :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
