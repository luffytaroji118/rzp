package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const solverUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// SolveRequest is the JSON body sent to the solver.
type SolveRequest struct {
	URL   string `json:"url"`
	Proxy string `json:"proxy"`
}

// SolveResponse is what the solver returns.
type SolveResponse struct {
	Charged     bool   `json:"charged"`
	PageText    string `json:"page_text"`
	ClosedEarly bool   `json:"closed_early"`
	Error       string `json:"error,omitempty"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/solve", handleSolve)

	log.Printf("Razorpay 3DS Solver listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func handleSolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, SolveResponse{Error: "read body failed"})
		return
	}

	var req SolveRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, SolveResponse{Error: "parse body failed"})
		return
	}

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, SolveResponse{Error: "url is required"})
		return
	}

	result := solve3DS(req.URL, req.Proxy)
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// solve3DS opens the 3DS redirect URL in a headless browser, waits for the
// bank page to process, reads the final page text, and determines if the
// payment was charged (frictionless 3DS).
func solve3DS(redirectURL, proxyURL string) SolveResponse {
	resp := SolveResponse{}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-web-security", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.WindowSize(1366, 768),
		chromedp.UserAgent(solverUserAgent),
	)

	if chromePath := findChrome(); chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	if proxyURL != "" {
		opts = append(opts, chromedp.ProxyServer(proxyURL))
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	browserCtx, cancelBrowser := context.WithTimeout(allocCtx, 25*time.Second)
	defer cancelBrowser()

	taskCtx, cancelTask := chromedp.NewContext(browserCtx)
	defer cancelTask()

	// Navigate to the 3DS redirect URL
	navigateErr := chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if err := network.Enable().Do(ctx); err != nil {
				return nil
			}
			return nil
		}),
		chromedp.Navigate(redirectURL),
	)

	if navigateErr != nil {
		errStr := navigateErr.Error()
		if strings.Contains(errStr, "Target closed") || strings.Contains(errStr, "browser has been closed") {
			resp.ClosedEarly = true
			return resp
		}
		_ = chromedp.Run(taskCtx, chromedp.Navigate(redirectURL))
	}

	// Wait for bank page to process (up to 10s)
	// Frictionless 3DS will auto-redirect back to Razorpay with a signature
	waitCtx, cancelWait := context.WithTimeout(taskCtx, 12*time.Second)
	defer cancelWait()

	_ = chromedp.Run(waitCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				var currentURL string
				_ = chromedp.Run(ctx, chromedp.Location(&currentURL))

				// If redirected away from pg_router, bank processed it
				if !strings.Contains(currentURL, "pg_router") && !strings.Contains(currentURL, "authenticate") {
					time.Sleep(500 * time.Millisecond)
					var pageText string
					_ = chromedp.Run(ctx, chromedp.Text("body", &pageText, chromedp.ByQuery))
					resp.PageText = strings.TrimSpace(pageText)
					lower := strings.ToLower(pageText)
					if strings.Contains(lower, "razorpay_signature") ||
						strings.Contains(lower, "payment successful") ||
						strings.Contains(lower, "payment_success") {
						resp.Charged = true
					}
					return nil
				}

				time.Sleep(500 * time.Millisecond)
			}
			return nil
		}),
	)

	// Read final page text if not already captured
	if resp.PageText == "" {
		var pageText string
		err := chromedp.Run(taskCtx, chromedp.Text("body", &pageText, chromedp.ByQuery))
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "Target closed") || strings.Contains(errStr, "browser has been closed") {
				resp.ClosedEarly = true
			}
		} else {
			resp.PageText = strings.TrimSpace(pageText)
			if len(resp.PageText) > 300 {
				resp.PageText = resp.PageText[:300]
			}
			lower := strings.ToLower(resp.PageText)
			if strings.Contains(lower, "razorpay_signature") ||
				strings.Contains(lower, "payment successful") ||
				strings.Contains(lower, "payment_success") {
				resp.Charged = true
			}
		}
	}

	return resp
}

// findChrome returns the path to Chrome/Chromium on the system.
func findChrome() string {
	candidates := []string{
		os.Getenv("CHROME_BIN"),
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/opt/google/chrome/chrome",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
