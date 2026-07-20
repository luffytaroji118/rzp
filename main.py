import asyncio
import json
import logging
import os
import platform
import traceback
from urllib.parse import urlparse

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from playwright.async_api import async_playwright

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(message)s",
)
log = logging.getLogger("solver")

MAX_CONCURRENCY = int(os.environ.get("MAX_CONCURRENCY", "2"))
TDS_WAIT_SECONDS = int(os.environ.get("TDS_WAIT_SECONDS", "15"))
NAV_TIMEOUT_MS = 15000
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

_semaphore = asyncio.Semaphore(MAX_CONCURRENCY)
_playwright = None
_browser_factory_lock = asyncio.Lock()

app = FastAPI()


def parse_proxy(proxy_url: str):
    """Parse http://user:pass@host:port into a Playwright proxy dict."""
    if not proxy_url:
        return None
    try:
        parsed = urlparse(proxy_url)
        if not parsed.hostname or not parsed.port:
            return None
        proxy = {"server": f"http://{parsed.hostname}:{parsed.port}"}
        if parsed.username:
            proxy["username"] = parsed.username
        if parsed.password:
            proxy["password"] = parsed.password
        return proxy
    except Exception as e:
        log.warning(f"parse_proxy failed for {proxy_url}: {e}")
        return None


def browser_args_for_platform():
    args = [
        "--no-sandbox",
        "--disable-dev-shm-usage",
        "--disable-web-security",
        "--disable-blink-features=AutomationControlled",
        "--disable-features=IsolateOrigins,site-per-process",
        "--ignore-certificate-errors",
        "--ignore-certificate-errors-spki-list",
    ]
    if platform.system() == "Linux":
        args.extend(["--disable-gpu", "--disable-software-rasterizer"])
    return args


async def solve_3ds(redirect_url: str, proxy_url: str) -> dict:
    """Navigate to the 3DS redirect URL, wait for the bank page, and return the result.

    Mirrors the v4-main _handle_3ds_redirect_with_cancel_sync logic:
      1. Launch Chromium with proxy (Playwright handles auth natively)
      2. page.goto(redirect_url, wait_until='domcontentloaded')
      3. Wait TDS_WAIT_SECONDS for the JS auto-submit + bank 3DS page to load
      4. Read body inner_text
      5. Detect 'razorpay_signature' / 'payment successful' => charged
    """
    proxy_config = parse_proxy(proxy_url)
    log.info(
        "solve_3ds start redirect=%s proxy=%s",
        redirect_url,
        proxy_config.get("server") if proxy_config else "none",
    )

    page_text = ""
    page_closed_early = False
    charged = False
    error_msg = ""

    browser = None
    try:
        async with async_playwright() as p:
            browser = await p.chromium.launch(
                headless=True,
                proxy=proxy_config,
                args=browser_args_for_platform(),
                timeout=NAV_TIMEOUT_MS,
            )
            context = await browser.new_context(
                ignore_https_errors=True,
                user_agent=USER_AGENT,
                viewport={"width": 1366, "height": 768},
            )
            page = await context.new_page()

            # Capture all network activity to diagnose chrome-error://chromewebdata/
            failed_requests = []
            all_responses = []
            all_requests = []

            def on_request_failed(req):
                try:
                    url = req.url
                    failure = req.failure
                    err_text = failure.error_text if failure else "unknown"
                    failed_requests.append(f"{url} -> {err_text}")
                    log.warning("solve_3ds REQUEST FAILED: %s -> %s", url, err_text)
                except Exception:
                    pass

            def on_pageerror(err):
                log.warning("solve_3ds PAGE ERROR: %s", err)

            def on_request(req):
                try:
                    all_requests.append(f"{req.method} {req.url}")
                    if req.resource_type == "document":
                        log.info("solve_3ds REQUEST: %s %s (document)", req.method, req.url)
                except Exception:
                    pass

            def on_response(resp):
                try:
                    all_responses.append(f"{resp.status} {resp.url}")
                    if resp.status >= 400:
                        log.warning("solve_3ds RESPONSE %d: %s", resp.status, resp.url)
                    elif resp.resource_type == "document":
                        log.info("solve_3ds RESPONSE %d: %s (document)", resp.status, resp.url)
                except Exception:
                    pass

            def onframenavigated(frame):
                try:
                    log.info("solve_3ds FRAME NAVIGATED: url=%s", frame.url)
                except Exception:
                    pass

            page.on("requestfailed", on_request_failed)
            page.on("pageerror", on_pageerror)
            page.on("request", on_request)
            page.on("response", on_response)
            page.on("framenavigated", onframenavigated)

            # CDP-level network error capture - Playwright's requestfailed
            # doesn't always fire for navigation failures, but CDP's
            # Network.loadingFailed includes the exact errorText
            # (e.g. ERR_PROXY_CONNECTION_FAILED, ERR_CERT_AUTHORITY_INVALID)
            try:
                cdp = await context.new_cdp_session(page)
                await cdp.send("Network.enable")

                def on_cdp_loading_failed(params):
                    err_text = params.get("errorText", "unknown")
                    req_url = params.get("requestId", "")
                    blocked_reason = params.get("blockedReason", "")
                    log.warning(
                        "solve_3ds CDP loadingFailed: errorText=%s blockedReason=%s requestId=%s",
                        err_text,
                        blocked_reason,
                        req_url,
                    )

                def on_cdp_response_received(params):
                    resp = params.get("response", {})
                    status = resp.get("status", 0)
                    url = resp.get("url", "")
                    if status >= 400:
                        log.warning("solve_3ds CDP response %d: %s", status, url)

                cdp.on("Network.loadingFailed", on_cdp_loading_failed)
                cdp.on("Network.responseReceived", on_cdp_response_received)
            except Exception as cdp_err:
                log.warning("solve_3ds CDP setup failed: %s", cdp_err)

            # Track popups (bank 3DS page may open in a new window)
            popup_pages = []

            def on_popup(popup):
                try:
                    log.info("solve_3ds POPUP opened url=%s", popup.url)
                    popup_pages.append(popup)
                    popup.on("framenavigated", lambda f: log.info("solve_3ds POPUP FRAME NAVIGATED: url=%s", f.url))
                    popup.on("requestfailed", on_request_failed)
                    popup.on("pageerror", on_pageerror)
                    popup.on("request", on_request)
                    popup.on("response", on_response)
                except Exception:
                    pass

            page.on("popup", on_popup)

            try:
                await page.goto(
                    redirect_url,
                    timeout=NAV_TIMEOUT_MS,
                    wait_until="domcontentloaded",
                )
                log.info("solve_3ds navigate OK")
                # Capture the initial page content right after navigation
                # (before JS auto-submit runs) to see if the pg_router form loaded
                try:
                    init_html = await page.content()
                    init_url = page.url
                    log.info(
                        "solve_3ds post-nav url=%s html_len=%d preview=%s",
                        init_url,
                        len(init_html),
                        init_html[:300] if init_html else "",
                    )
                except Exception as e:
                    log.warning("solve_3ds post-nav capture error: %s", e)
            except Exception as nav_err:
                err_str = str(nav_err)
                log.warning("solve_3ds navigate error: %s", err_str)
                if "Target closed" in err_str or "browser has been closed" in err_str:
                    page_closed_early = True
                else:
                    # Retry after a short delay - ERR_TUNNEL_CONNECTION_FAILED
                    # is often transient (proxy connection exhaustion)
                    await page.wait_for_timeout(2000)
                    try:
                        await page.goto(
                            redirect_url,
                            timeout=12000,
                            wait_until="commit",
                        )
                        log.info("solve_3ds navigate retry OK")
                    except Exception as retry_err:
                        log.warning("solve_3ds navigate retry failed: %s", retry_err)

            # Poll for bank page content across main page, all frames, and popups.
            # The pg_router page auto-submits a form after ~8s that opens the bank
            # 3DS page (sometimes in a popup, sometimes in an iframe, sometimes as
            # a full navigation). A single blind wait misses it; we poll instead.
            if not page_closed_early:
                import time as _time
                deadline = _time.monotonic() + TDS_WAIT_SECONDS
                last_url = ""
                poll_count = 0
                while _time.monotonic() < deadline:
                    poll_count += 1
                    try:
                        await page.wait_for_timeout(1000)
                    except Exception:
                        pass

                    candidates = [page] + list(popup_pages)
                    for cand in candidates:
                        try:
                            if cand.is_closed():
                                continue
                            cand_url = cand.url
                            # Log URL changes
                            if cand_url != last_url:
                                log.info("solve_3ds poll#%d url=%s", poll_count, cand_url)
                                last_url = cand_url

                            # Check main frame body text
                            try:
                                body = await cand.locator("body").inner_text(timeout=2000)
                            except Exception:
                                body = ""
                            body = (body or "").strip()

                            # Check all child frames
                            if not body:
                                for frame in cand.frames:
                                    if frame == cand.main_frame:
                                        continue
                                    try:
                                        fbody = await frame.locator("body").inner_text(timeout=1500)
                                        fbody = (fbody or "").strip()
                                        if fbody:
                                            log.info(
                                                "solve_3ds poll#%d frame url=%s textLen=%d preview=%s",
                                                poll_count,
                                                frame.url,
                                                len(fbody),
                                                fbody[:80],
                                            )
                                            body = fbody
                                            break
                                    except Exception:
                                        continue

                            if body:
                                lower = body.lower()
                                # Detect charge on any page (pg_router or bank)
                                if "razorpay_signature" in lower or "payment successful" in lower or "payment_success" in lower or "payment succeeded" in lower:
                                    charged = True
                                    page_text = body
                                    log.info("solve_3ds CHARGED detected at poll#%d", poll_count)
                                    break
                                # If we're still on pg_router/about:blank, do NOT break -
                                # keep polling for the bank page (form takes ~8s to submit)
                                if "pg_router" in cand_url or "about:blank" in cand_url or "authenticate" in cand_url:
                                    log.info(
                                        "solve_3ds poll#%d still on pg_router url=%s textLen=%d",
                                        poll_count,
                                        cand_url,
                                        len(body),
                                    )
                                    continue
                                # We're past pg_router - bank page loaded. Capture it.
                                log.info(
                                    "solve_3ds poll#%d bank page detected url=%s textLen=%d preview=%s",
                                    poll_count,
                                    cand_url,
                                    len(body),
                                    body[:80],
                                )
                                # Give it 2 more seconds to fully render
                                try:
                                    await cand.wait_for_timeout(2000)
                                    body = await cand.locator("body").inner_text(timeout=3000)
                                    body = (body or "").strip()
                                except Exception:
                                    pass
                                page_text = body
                                break
                        except Exception:
                            continue

                    if charged or page_text:
                        break

                log.info(
                    "solve_3ds poll ended after %d polls textLen=%d charged=%s",
                    poll_count,
                    len(page_text or ""),
                    charged,
                )

            # Final diagnostics: capture URL, title, HTML from main page + popups
            try:
                final_url = page.url
                title = await page.title()
                log.info("solve_3ds final url=%s title=%s", final_url, title)

                # Also log popup states
                for i, popup in enumerate(popup_pages):
                    try:
                        if popup.is_closed():
                            log.info("solve_3ds popup#%d CLOSED", i)
                        else:
                            p_url = popup.url
                            p_title = await popup.title()
                            log.info("solve_3ds popup#%d url=%s title=%s", i, p_url, p_title)
                    except Exception:
                        pass

                if not page_text:
                    html = await page.content()
                    log.warning(
                        "solve_3ds no text - main html_len=%d preview=%s",
                        len(html),
                        html[:500] if html else "",
                    )
                    # Check popup HTML too
                    for i, popup in enumerate(popup_pages):
                        try:
                            if not popup.is_closed():
                                p_html = await popup.content()
                                log.warning(
                                    "solve_3ds popup#%d html_len=%d preview=%s",
                                    i,
                                    len(p_html),
                                    p_html[:300] if p_html else "",
                                )
                        except Exception:
                            pass
                    if failed_requests:
                        log.warning(
                            "solve_3ds FAILED REQUESTS: %s",
                            "; ".join(failed_requests[:5]),
                        )
            except Exception as diag_err:
                log.warning("solve_3ds diag error: %s", diag_err)

            try:
                await browser.close()
            except Exception:
                pass
            browser = None

    except Exception as e:
        err_str = str(e)
        if "Target closed" in err_str or "browser has been closed" in err_str:
            page_closed_early = True
        else:
            error_msg = err_str
            log.error("solve_3ds exception: %s\n%s", e, traceback.format_exc())
    finally:
        if browser is not None:
            try:
                await browser.close()
            except Exception:
                pass

    result = {
        "charged": charged,
        "page_text": (page_text or "").strip()[:300],
        "closed_early": page_closed_early,
    }
    if error_msg:
        result["error"] = error_msg
    log.info(
        "solve_3ds done charged=%s closed_early=%s textLen=%d",
        charged,
        page_closed_early,
        len(result["page_text"]),
    )
    return result


@app.on_event("startup")
async def startup_event():
    global _playwright
    _playwright = await async_playwright().start()
    log.info(
        "Razorpay 3DS Solver (Playwright) starting on PORT=%s max_concurrency=%d tds_wait=%ds",
        os.environ.get("PORT", "8080"),
        MAX_CONCURRENCY,
        TDS_WAIT_SECONDS,
    )


@app.on_event("shutdown")
async def shutdown_event():
    global _playwright
    if _playwright:
        await _playwright.stop()
    log.info("Solver shutdown complete")


@app.get("/health")
async def health():
    return {"status": "ok"}


@app.post("/solve")
async def solve(request: Request):
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    url = body.get("url", "")
    proxy = body.get("proxy", "")
    if not url:
        return JSONResponse({"error": "missing url"}, status_code=400)

    # Non-blocking acquire: return 503 immediately if all slots are busy.
    try:
        async with asyncio.timeout(0.01):
            await _semaphore.acquire()
    except (asyncio.TimeoutError, TimeoutError):
        return JSONResponse({"error": "all solver slots busy"}, status_code=503)

    try:
        result = await asyncio.wait_for(solve_3ds(url, proxy), timeout=60)
        return JSONResponse(result)
    except asyncio.TimeoutError:
        return JSONResponse({"error": "solver timeout"}, status_code=504)
    except Exception as e:
        log.error("solve endpoint error: %s\n%s", e, traceback.format_exc())
        return JSONResponse({"error": str(e)}, status_code=500)
    finally:
        _semaphore.release()


if __name__ == "__main__":
    import uvicorn

    port = int(os.environ.get("PORT", "8080"))
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="info")
