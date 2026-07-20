package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/cdproto/fetch"
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

// proxyInfo holds parsed proxy details for Chrome + CDP auth.
type proxyInfo struct {
	server   string // host:port for Chrome --proxy-server
	username string
	password string
	hasAuth  bool
}

func parseProxy(proxyURL string) *proxyInfo {
	if proxyURL == "" {
		return nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil
	}
	pi := &proxyInfo{
		server:  parsed.Host,
		hasAuth: parsed.User != nil,
	}
	if pi.hasAuth {
		pi.username = parsed.User.Username()
		pi.password, _ = parsed.User.Password()
	}
	if pi.server == "" {
		return nil
	}
	return pi
}

// startChrome launches a headless Chrome process with remote debugging enabled
// and returns the process handle, the DevTools WebSocket URL, the temp user-data-dir
// (for cleanup), and any error.
//
// We manage the Chrome process manually instead of using chromedp's ExecAllocator
// to avoid the "close of closed channel" panic that occurs in chromedp's internal
// goroutines when the allocator is canceled.
func startChrome(pi *proxyInfo) (*exec.Cmd, string, string, error) {
	chromePath := findChrome()
	if chromePath == "" {
		return nil, "", "", fmt.Errorf("chrome not found")
	}

	tmpDir, err := os.MkdirTemp("", "chrome-solver-")
	if err != nil {
		return nil, "", "", fmt.Errorf("create temp dir: %w", err)
	}

	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-web-security",
		"--disable-blink-features=AutomationControlled",
		"--disable-gpu",
		"--ignore-certificate-errors",
		"--disable-extensions",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-popup-blocking",
		"--window-size=1366,768",
		"--user-agent=" + solverUserAgent,
		"--remote-debugging-port=0",
		"--user-data-dir=" + tmpDir,
	}

	if pi != nil && pi.server != "" {
		args = append(args, "--proxy-server="+pi.server)
	}

	cmd := exec.Command(chromePath, args...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, "", "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, "", "", fmt.Errorf("start chrome: %w", err)
	}

	wsURLChan := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "ws://"); idx >= 0 {
				if strings.Contains(line, "DevTools") || strings.Contains(line, "listening") {
					wsURLChan <- strings.TrimSpace(line[idx:])
					return
				}
			}
		}
	}()

	select {
	case wsURL := <-wsURLChan:
		return cmd, wsURL, tmpDir, nil
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
		os.RemoveAll(tmpDir)
		return nil, "", "", fmt.Errorf("timeout waiting for Chrome DevTools URL")
	}
}

// solve3DS opens the 3DS redirect URL in a headless browser, waits for the
// bank page to process, reads the final page text, and determines if the
// payment was charged (frictionless 3DS).
//
// The 3DS flow has multiple phases:
//  1. Razorpay "Loading Bank page…" — redirect to bank in progress
//  2. Bank 3DS page — frictionless auto-approves or shows OTP challenge
//  3. Redirect back to Razorpay — success or failure page
//
// We poll for up to 35 seconds, checking both URL and page text at each
// iteration. We exit early on definitive success/failure indicators.
//
// A fresh Chrome allocator is created per request to avoid the
// "close of closed channel" panic that occurs when a shared
// allocator is used by concurrent goroutines.
func solve3DS(redirectURL, proxyURL string) (resp SolveResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			errMsg := fmt.Sprintf("chromedp panic recovered: %v", rec)
			log.Printf("PANIC in solve3DS: %s", errMsg)
			resp.Error = errMsg
		}
	}()

	pi := parseProxy(proxyURL)
	if pi != nil {
		log.Printf("solve3DS: proxy=%s hasAuth=%v redirect=%s", pi.server, pi.hasAuth, redirectURL)
	} else {
		log.Printf("solve3DS: no proxy redirect=%s", redirectURL)
	}

	cmd, wsURL, tmpDir, err := startChrome(pi)
	if err != nil {
		resp.Error = fmt.Sprintf("start chrome: %v", err)
		log.Printf("solve3DS: startChrome failed: %v", err)
		return resp
	}
	log.Printf("solve3DS: chrome started wsURL=%s", wsURL)
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
		os.RemoveAll(tmpDir)
	}()

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), wsURL)
	defer cancelAlloc()

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()

	if pi != nil && pi.hasAuth {
		chromedp.ListenTarget(taskCtx, func(ev interface{}) {
			switch ev := ev.(type) {
			case *fetch.EventAuthRequired:
				go func() {
					_ = fetch.ContinueWithAuth(ev.RequestID, &fetch.AuthChallengeResponse{
						Response: fetch.AuthChallengeResponseResponseProvideCredentials,
						Username: pi.username,
						Password: pi.password,
					}).Do(taskCtx)
				}()
			case *fetch.EventRequestPaused:
				go func() {
					_ = fetch.ContinueRequest(ev.RequestID).Do(taskCtx)
				}()
			}
		})
	}

	browserCtx, cancelBrowser := context.WithTimeout(taskCtx, 40*time.Second)
	defer cancelBrowser()

	navigateErr := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if err := network.Enable().Do(ctx); err != nil {
				log.Printf("solve3DS: network.Enable error: %v", err)
			}
			if pi != nil && pi.hasAuth {
				if err := fetch.Enable().WithHandleAuthRequests(true).Do(ctx); err != nil {
					log.Printf("solve3DS: fetch.Enable error: %v", err)
				} else {
					log.Printf("solve3DS: fetch.Enable OK (auth challenges will be handled)")
				}
			}
			return nil
		}),
		chromedp.Navigate(redirectURL),
	)

	if navigateErr != nil {
		log.Printf("solve3DS: navigate error: %v", navigateErr)
		errStr := navigateErr.Error()
		if strings.Contains(errStr, "Target closed") || strings.Contains(errStr, "browser has been closed") {
			resp.ClosedEarly = true
			return resp
		}
		_ = chromedp.Run(browserCtx, chromedp.Navigate(redirectURL))
	} else {
		log.Printf("solve3DS: navigate OK")
	}

	waitCtx, cancelWait := context.WithTimeout(browserCtx, 37*time.Second)
	defer cancelWait()

	_ = chromedp.Run(waitCtx,
		chromedp.ActionFunc(func(pollCtx context.Context) error {
			deadline := time.Now().Add(36 * time.Second)
			leftPgRouter := false
			leftPgRouterTime := time.Time{}

			for time.Now().Before(deadline) {
				select {
				case <-pollCtx.Done():
					return pollCtx.Err()
				default:
				}

				var currentURL string
				if err := chromedp.Run(pollCtx, chromedp.Location(&currentURL)); err != nil {
					log.Printf("solve3DS: poll Location error: %v", err)
				}

				var pageText string
				if err := chromedp.Run(pollCtx, chromedp.Text("body", &pageText, chromedp.ByQuery)); err != nil {
					log.Printf("solve3DS: poll Text error: %v", err)
				}
				pageText = strings.TrimSpace(pageText)
				lower := strings.ToLower(pageText)

				if pageText != "" && len(pageText) > 0 {
					log.Printf("solve3DS: poll url=%s textLen=%d textPreview=%s", currentURL, len(pageText), truncateText(pageText, 80))
				}

				if pageText != "" {
					if strings.Contains(lower, "razorpay_signature") ||
						strings.Contains(lower, "payment successful") ||
						strings.Contains(lower, "payment_success") ||
						strings.Contains(lower, "payment succeeded") ||
						strings.Contains(lower, "payment_done") {
						resp.Charged = true
						resp.PageText = truncateText(pageText, 300)
						return nil
					}
					if strings.Contains(lower, "payment") && strings.Contains(lower, "failed") {
						resp.PageText = truncateText(pageText, 300)
						return nil
					}
					if strings.Contains(lower, "transaction failed") ||
						strings.Contains(lower, "authentication failed") ||
						strings.Contains(lower, "access denied") {
						resp.PageText = truncateText(pageText, 300)
						return nil
					}
				}

				onPgRouter := strings.Contains(currentURL, "pg_router") || strings.Contains(currentURL, "authenticate")
				if !onPgRouter && pageText != "" {
					if !leftPgRouter {
						leftPgRouter = true
						leftPgRouterTime = time.Now()
					}
					if time.Since(leftPgRouterTime) > 3*time.Second {
						resp.PageText = truncateText(pageText, 300)
						return nil
					}
				}

				time.Sleep(500 * time.Millisecond)
			}

			var finalText string
			if err := chromedp.Run(pollCtx, chromedp.Text("body", &finalText, chromedp.ByQuery)); err != nil {
				log.Printf("solve3DS: final Text error: %v", err)
			}

			var finalURL string
			_ = chromedp.Run(pollCtx, chromedp.Location(&finalURL))
			log.Printf("solve3DS: poll ended url=%s textLen=%d", finalURL, len(finalText))

			var finalHTML string
			_ = chromedp.Run(pollCtx, chromedp.OuterHTML("html", &finalHTML, chromedp.ByQuery))
			if finalHTML != "" {
				log.Printf("solve3DS: final HTML len=%d preview=%s", len(finalHTML), truncateText(finalHTML, 200))
			} else {
				log.Printf("solve3DS: final HTML empty - Chrome may not have loaded any page")
			}

			finalText = strings.TrimSpace(finalText)
			if finalText != "" {
				resp.PageText = truncateText(finalText, 300)
				lower := strings.ToLower(finalText)
				if strings.Contains(lower, "razorpay_signature") ||
					strings.Contains(lower, "payment successful") ||
					strings.Contains(lower, "payment_success") ||
					strings.Contains(lower, "payment succeeded") {
					resp.Charged = true
				}
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
			resp.PageText = truncateText(strings.TrimSpace(pageText), 300)
			lower := strings.ToLower(resp.PageText)
			if strings.Contains(lower, "razorpay_signature") ||
				strings.Contains(lower, "payment successful") ||
				strings.Contains(lower, "payment_success") ||
				strings.Contains(lower, "payment succeeded") {
				resp.Charged = true
			}
		}
	}

	return resp
}

func truncateText(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
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
