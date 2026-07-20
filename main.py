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
TDS_WAIT_SECONDS = int(os.environ.get("TDS_WAIT_SECONDS", "10"))
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

            try:
                await page.goto(
                    redirect_url,
                    timeout=NAV_TIMEOUT_MS,
                    wait_until="domcontentloaded",
                )
                log.info("solve_3ds navigate OK")
            except Exception as nav_err:
                err_str = str(nav_err)
                log.warning("solve_3ds navigate error: %s", err_str)
                if "Target closed" in err_str or "browser has been closed" in err_str:
                    page_closed_early = True
                else:
                    try:
                        await page.goto(
                            redirect_url,
                            timeout=12000,
                            wait_until="commit",
                        )
                        log.info("solve_3ds navigate retry OK")
                    except Exception as retry_err:
                        log.warning("solve_3ds navigate retry failed: %s", retry_err)

            if not page_closed_early:
                try:
                    await page.wait_for_timeout(TDS_WAIT_SECONDS * 1000)
                    page_text = await page.locator("body").inner_text()
                    log.info(
                        "solve_3ds after wait textLen=%d preview=%s",
                        len(page_text),
                        page_text[:120] if page_text else "",
                    )
                except Exception as wait_err:
                    err_str = str(wait_err)
                    if "Target closed" in err_str or "browser has been closed" in err_str:
                        page_closed_early = True
                        log.info("solve_3ds page closed early during wait")
                    else:
                        try:
                            page_text = await page.locator("body").inner_text()
                        except Exception:
                            page_text = ""

            lower = (page_text or "").lower()
            if "razorpay_signature" in lower or "payment successful" in lower or "payment_success" in lower or "payment succeeded" in lower:
                charged = True
                log.info("solve_3ds CHARGED detected")

            try:
                current_url = page.url
                log.info("solve_3ds final url=%s", current_url)
            except Exception:
                pass

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
