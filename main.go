package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	solverUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	maxConcurrency  = 2
)

var semaphore = make(chan struct{}, maxConcurrency)

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
	mux.HandleFunc("/solve", recoveryMiddleware(handleSolve))

	log.Printf("Razorpay 3DS Solver listening on :%s (max concurrency: %d)", port, maxConcurrency)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func recoveryMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				errMsg := fmt.Sprintf("panic recovered: %v", rec)
				log.Printf("PANIC: %s", errMsg)
				writeJSON(w, http.StatusInternalServerError, SolveResponse{Error: errMsg})
			}
		}()
		next(w, r)
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

	select {
	case semaphore <- struct{}{}:
	default:
		writeJSON(w, http.StatusServiceUnavailable, SolveResponse{Error: "busy"})
		return
	}
	defer func() { <-semaphore }()

	result := solve3DS(req.URL, req.Proxy)
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

var (
	allocCtx   context.Context
	allocCancel context.CancelFunc
	allocOnce  sync.Once
)

func initAllocator() {
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

	allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
}

// solve3DS opens the 3DS redirect URL in a headless browser, waits for the
// bank page to process, reads the final page text, and determines if the
// payment was charged (frictionless 3DS).
func solve3DS(redirectURL, proxyURL string) (resp SolveResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			errMsg := fmt.Sprintf("chromedp panic recovered: %v", rec)
			log.Printf("PANIC in solve3DS: %s", errMsg)
			resp.Error = errMsg
		}
	}()

	allocOnce.Do(initAllocator)

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()

	browserCtx, cancelBrowser := context.WithTimeout(taskCtx, 10*time.Second)
	defer cancelBrowser()

	navigateErr := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			network.Enable().Do(ctx)
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
		_ = chromedp.Run(browserCtx, chromedp.Navigate(redirectURL))
	}

	waitCtx, cancelWait := context.WithTimeout(browserCtx, 6*time.Second)
	defer cancelWait()

	_ = chromedp.Run(waitCtx,
		chromedp.ActionFunc(func(pollCtx context.Context) error {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-pollCtx.Done():
					return pollCtx.Err()
				default:
				}

				var currentURL string
				_ = chromedp.Run(pollCtx, chromedp.Location(&currentURL))

				if !strings.Contains(currentURL, "pg_router") && !strings.Contains(currentURL, "authenticate") {
					var pageText string
					_ = chromedp.Run(pollCtx, chromedp.Text("body", &pageText, chromedp.ByQuery))
					resp.PageText = strings.TrimSpace(pageText)
					lower := strings.ToLower(pageText)
					if strings.Contains(lower, "razorpay_signature") ||
						strings.Contains(lower, "payment successful") ||
						strings.Contains(lower, "payment_success") {
						resp.Charged = true
					}
					return nil
				}

				time.Sleep(300 * time.Millisecond)
			}
			return nil
		}),
	)

	if resp.PageText == "" {
		var pageText string
		err := chromedp.Run(browserCtx, chromedp.Text("body", &pageText, chromedp.ByQuery))
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
