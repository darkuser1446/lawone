package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────
// DATA STRUCTURES
// ─────────────────────────────────────────

type StoryRequest struct {
	Story string `json:"story"`
}

type VFT struct {
	Entity      string `json:"entity"`
	Agreement   string `json:"agreement"`
	PaymentMade string `json:"payment_made"`
	Performance string `json:"performance"`
	HasReceipt  string `json:"has_receipt"`
	NoticeSent  string `json:"notice_sent"`
}

type LawoneNode struct {
	Node        string  `json:"node"`
	Requirement string  `json:"requirement"`
	Status      string  `json:"status"`
	Score       float64 `json:"score"`
}

type LawoneResponse struct {
	LawoneScore     float64      `json:"lawone_score"`
	Strength        string       `json:"strength"`
	Nodes           []LawoneNode `json:"nodes"`
	MissingQuestion string       `json:"missing_question"`
	VFT             *VFT         `json:"vft"`
}

// ─────────────────────────────────────────
// GEMINI CALL
// ─────────────────────────────────────────

func callGemini(story string) (*VFT, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("missing GEMINI_API_KEY")
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=%s",
		apiKey,
	)

	systemPrompt := `You are a legal fact extractor.

Return ONLY JSON:
{
  "entity": "name or Unknown",
  "agreement": "Found or Not Found",
  "payment_made": "₹amount or None",
  "performance": "Completed or Partial or Not started or Unknown",
  "has_receipt": "Yes or No or Unknown",
  "notice_sent": "Yes or No or Unknown"
}`

	body := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": systemPrompt + "\n\nStory:\n" + story},
				},
			},
		},
	}

	bodyBytes, _ := json.Marshal(body)

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
		if err != nil {
			return nil, err
		}

		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			continue
		}

		var geminiResp map[string]interface{}
		if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
			continue
		}

		candidates, ok := geminiResp["candidates"].([]interface{})
		if !ok || len(candidates) == 0 {
			continue
		}

		content := candidates[0].(map[string]interface{})["content"].(map[string]interface{})
		parts := content["parts"].([]interface{})
		rawText := parts[0].(map[string]interface{})["text"].(string)

		rawText = strings.TrimSpace(rawText)
		rawText = strings.Trim(rawText, "`")

		var vft VFT
		if err := json.Unmarshal([]byte(rawText), &vft); err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		return &vft, nil
	}

	return nil, fmt.Errorf("Gemini failed")
}

// ─────────────────────────────────────────
// LOGIC
// ─────────────────────────────────────────

func buildLawoneScore(vft *VFT) LawoneResponse {
	nodes := []LawoneNode{}
	total := 0.0
	missingQ := ""

	if vft.Entity != "" && vft.Entity != "Unknown" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node A", "Identity", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node A", "Identity", "Pending", 0})
		missingQ = "Who is the other party?"
	}

	if vft.PaymentMade != "" && vft.PaymentMade != "None" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node B", "Payment", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node B", "Payment", "Pending", 0})
	}

	if vft.Performance == "Partial" || vft.Performance == "Not started" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node C", "Breach", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node C", "Breach", "Pending", 0})
	}

	if vft.NoticeSent == "Yes" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node D", "Notice", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node D", "Notice", "Pending", 0})
	}

	strength := "Weak"
	if total >= 0.75 {
		strength = "Strong"
	} else if total >= 0.5 {
		strength = "Medium"
	}

	return LawoneResponse{
		LawoneScore:     total,
		Strength:        strength,
		Nodes:           nodes,
		MissingQuestion: missingQ,
		VFT:             vft,
	}
}

// ─────────────────────────────────────────
// DB
// ─────────────────────────────────────────

func initDB(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS cases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		story TEXT,
		score REAL
	)`)
}

// ─────────────────────────────────────────
// HANDLER
// ─────────────────────────────────────────

func analyze(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req StoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}

		vft, err := callGemini(req.Story)
		if err != nil {
			http.Error(w, `{"error":"AI failed"}`, http.StatusInternalServerError)
			return
		}

		result := buildLawoneScore(vft)

		db.Exec(`INSERT INTO cases (story, score) VALUES (?, ?)`, req.Story, result.LawoneScore)

		json.NewEncoder(w).Encode(result)
	}
}

// ─────────────────────────────────────────
// MAIN
// ─────────────────────────────────────────

func main() {
	db, err := sql.Open("sqlite", "./lawone.db")
	if err != nil {
		log.Fatal(err)
	}
	initDB(db)

	// ✅ API
	http.HandleFunc("/analyze", analyze(db))

	// ✅ FRONTEND (IMPORTANT)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "lawone.html")
	})

	// ✅ PORT FIX (IMPORTANT)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Println("🚀 Running on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}