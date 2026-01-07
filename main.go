package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Estruturas
type EmailEntry struct {
	ID        int       `json:"id"`
	Alias     string    `json:"alias"`
	RuleID    string    `json:"rule_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"` // Novo campo
	Status    string    `json:"status"`
}

type CFRequest struct {
	Matchers []CFMatcher `json:"matchers"`
	Actions  []CFAction  `json:"actions"`
	Enabled  bool        `json:"enabled"`
	Name     string      `json:"name"`
}

type CFMatcher struct {
	Type  string `json:"type"`
	Field string `json:"field"`
	Value string `json:"value"`
}

type CFAction struct {
	Type  string   `json:"type"`
	Value []string `json:"value"`
}

type CFResponse struct {
	Success bool `json:"success"`
	Result  struct {
		ID string `json:"id"`
	} `json:"result"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

var db *sql.DB

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8086"
	}

	initDB()

	// Inicia o worker de limpeza em background
	go startCleanupWorker()

	// Rotas
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/generate", handleGenerate)
	http.HandleFunc("/api/toggle", handleToggle)
	http.HandleFunc("/api/delete", handleDelete)
	http.HandleFunc("/api/recreate", handleRecreate)
	http.HandleFunc("/api/renew", handleRenew) // Nova rota

	log.Printf("Servidor rodando na porta %s (Tabler UI)...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func initDB() {
	var err error
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/emails.db"
	}

	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}

	// Cria tabela se não existir
	query := `
	CREATE TABLE IF NOT EXISTS emails (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		alias TEXT NOT NULL,
		rule_id TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME,
		status TEXT DEFAULT 'active'
	);`
	_, err = db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}

	// Migração simples: Tenta adicionar a coluna expires_at caso o banco já exista sem ela
	// Ignora erro se a coluna já existir
	db.Exec("ALTER TABLE emails ADD COLUMN expires_at DATETIME")
}

// --- WORKER DE LIMPEZA ---
func startCleanupWorker() {
	ticker := time.NewTicker(1 * time.Minute)
	log.Println("Iniciando monitoramento de expiração de emails...")
	for range ticker.C {
		checkExpiredEmails()
	}
}

func checkExpiredEmails() {
	// Busca emails ativos que já venceram
	rows, err := db.Query("SELECT id, rule_id, alias FROM emails WHERE status = 'active' AND expires_at < datetime('now')")
	if err != nil {
		log.Println("Erro ao verificar expiração:", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var ruleID, alias string
		if err := rows.Scan(&id, &ruleID, &alias); err != nil {
			continue
		}

		log.Printf("Expirando email automaticamente: %s", alias)

		// Remove da Cloudflare
		if ruleID != "" {
			deleteCFRule(ruleID)
		}

		// Marca como deletado no banco
		db.Exec("UPDATE emails SET status = 'deleted', rule_id = '' WHERE id = ?", id)
	}
}

// --- HANDLERS ---

func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Ordena por status (ativos primeiro) e depois por data
	rows, err := db.Query(`
		SELECT id, alias, rule_id, created_at, IFNULL(expires_at, created_at), status 
		FROM emails 
		ORDER BY CASE WHEN status='active' THEN 1 ELSE 2 END, created_at DESC
	`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var emails []EmailEntry
	for rows.Next() {
		var e EmailEntry
		rows.Scan(&e.ID, &e.Alias, &e.RuleID, &e.CreatedAt, &e.ExpiresAt, &e.Status)
		emails = append(emails, e)
	}

	tmpl.Execute(w, emails)
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	aliasPrefix := generateRandomString(8)
	domain := os.Getenv("CF_EMAIL_DOMAIN")
	fullEmail := fmt.Sprintf("%s@%s", aliasPrefix, domain)

	ruleID, err := createCFRule(fullEmail, true)
	if err != nil {
		http.Error(w, "Erro Cloudflare: "+err.Error(), 500)
		return
	}

	// Define expiração para 1 hora a partir de agora
	expiresAt := time.Now().Add(1 * time.Hour)

	_, err = db.Exec("INSERT INTO emails (alias, rule_id, status, expires_at) VALUES (?, ?, 'active', ?)", fullEmail, ruleID, expiresAt)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleRenew(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	
	// Adiciona 1 hora ao tempo de expiração atual
	_, err := db.Exec("UPDATE emails SET expires_at = datetime(expires_at, '+1 hour') WHERE id = ? AND status = 'active'", id)
	if err != nil {
		log.Println("Erro ao renovar:", err)
	}
	
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var ruleID, status string
	err := db.QueryRow("SELECT rule_id, status FROM emails WHERE id = ?", id).Scan(&ruleID, &status)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	newStatus := "active"
	cfEnabled := true
	if status == "active" {
		newStatus = "inactive"
		cfEnabled = false
	}

	err = updateCFRule(ruleID, cfEnabled)
	if err != nil {
		http.Error(w, "Erro ao atualizar CF: "+err.Error(), 500)
		return
	}

	db.Exec("UPDATE emails SET status = ? WHERE id = ?", newStatus, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var ruleID string
	db.QueryRow("SELECT rule_id FROM emails WHERE id = ?", id).Scan(&ruleID)

	if ruleID != "" {
		deleteCFRule(ruleID)
	}

	db.Exec("UPDATE emails SET status = 'deleted', rule_id = '' WHERE id = ?", id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleRecreate(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var alias string
	db.QueryRow("SELECT alias FROM emails WHERE id = ?", id).Scan(&alias)

	ruleID, err := createCFRule(alias, true)
	if err != nil {
		http.Error(w, "Erro ao recriar: "+err.Error(), 500)
		return
	}

	// Ao recriar, reseta o timer para 1 hora
	expiresAt := time.Now().Add(1 * time.Hour)
	db.Exec("UPDATE emails SET status = 'active', rule_id = ?, expires_at = ? WHERE id = ?", ruleID, expiresAt, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- CLOUDFLARE HELPERS (Mesmos de antes) ---

func createCFRule(email string, enabled bool) (string, error) {
	dest := os.Getenv("CF_DESTINATION_EMAIL")
	zoneID := os.Getenv("CF_ZONE_ID")

	reqBody := CFRequest{
		Matchers: []CFMatcher{{Type: "literal", Field: "to", Value: email}},
		Actions:  []CFAction{{Type: "forward", Value: []string{dest}}},
		Enabled:  enabled,
		Name:     "TempMail-" + email,
	}

	return callCFAPI("POST", fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/email/routing/rules", zoneID), reqBody)
}

func updateCFRule(ruleID string, enabled bool) error {
	zoneID := os.Getenv("CF_ZONE_ID")
	payload := map[string]interface{}{"enabled": enabled}
	_, err := callCFAPI("PATCH", fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/email/routing/rules/%s", zoneID, ruleID), payload)
	return err
}

func deleteCFRule(ruleID string) error {
	zoneID := os.Getenv("CF_ZONE_ID")
	_, err := callCFAPI("DELETE", fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/email/routing/rules/%s", zoneID, ruleID), nil)
	return err
}

func callCFAPI(method, url string, body interface{}) (string, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewBuffer(jsonBytes)
	}

	req, _ := http.NewRequest(method, url, bodyReader)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("CF_API_TOKEN"))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	
	var cfResp CFResponse
	json.Unmarshal(respBytes, &cfResp)

	if !cfResp.Success && method != "DELETE" {
		if len(cfResp.Errors) > 0 {
			return "", fmt.Errorf(cfResp.Errors[0].Message)
		}
		return "", fmt.Errorf("unknown error from cloudflare")
	}

	return cfResp.Result.ID, nil
}

func generateRandomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Seed(time.Now().UnixNano())
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}