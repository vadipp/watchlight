// Optional end-to-end check. Set PLAYWRIGHT_MODULE to an installed playwright-core.
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const http = require("node:http");
const crypto = require("node:crypto");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
(async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "watchlight-smoke-"));
  let proc, browser, fixture;
  const errors = [];
  try {
    fixture = http.createServer((q, r) => r.end("healthy"));
    await new Promise((r) => fixture.listen(0, "127.0.0.1", r));
    const target = fixture.address().port;
    const probe = http.createServer();
    await new Promise((r) => probe.listen(0, "127.0.0.1", r));
    const port = probe.address().port;
    await new Promise((r) => probe.close(r));
    const password = crypto.randomBytes(24).toString("hex"),
      token = crypto.randomBytes(24).toString("hex");
    const config = path.join(dir, "config.yaml");
    fs.writeFileSync(
      config,
      `listen: 127.0.0.1:${port}\ndatabase: ${dir}/monitor.db\nowner_user: owner\nowner_password_env: SMOKE_PASSWORD\nmonitors:\n  - id: web\n    name: Public website\n    type: http\n    target: http://127.0.0.1:${target}\n    public: true\n    interval: 1s\n    timeout: 1s\n    failures: 2\n  - id: ha\n    name: Private Home Assistant\n    type: heartbeat\n    token_env: SMOKE_TOKEN\n    interval: 1s\n    timeout: 1s\n    max_age: 2s\n    failures: 2\n`,
      { mode: 0o600 },
    );
    proc = spawn(path.resolve("watchlight"), ["serve", "-config", config], {
      env: { ...process.env, SMOKE_PASSWORD: password, SMOKE_TOKEN: token },
      stdio: ["ignore", "ignore", "pipe"],
    });
    let logs = "";
    proc.stderr.on("data", (b) => (logs += b));
    const base = `http://127.0.0.1:${port}`;
    let started = false;
    for (let i = 0; i < 80; i++) {
      try {
        if ((await fetch(base)).ok) {
          started = true;
          break;
        }
      } catch {}
      await sleep(100);
    }
    if (!started) throw Error("startup failed " + logs);
    const auth = {
      Authorization:
        "Basic " + Buffer.from("owner:" + password).toString("base64"),
    };
    const assert = (v, msg) => {
      if (!v) throw Error(msg);
    };
    const pub = await (await fetch(base + "/api/public")).json();
    assert(pub.length === 1 && pub[0].id === "web", "private monitor leaked");
    assert(
      (await fetch(base + "/api/private")).status === 401,
      "private API accessible",
    );
    await sleep(2200);
    let privateData = await (
      await fetch(base + "/api/private", { headers: auth })
    ).json();
    assert(
      privateData.find((v) => v.id === "ha").state.status === "down",
      "missing heartbeat did not fail",
    );
    assert(
      (
        await fetch(base + "/api/heartbeat/ha", {
          method: "POST",
          headers: { Authorization: "Bearer " + token },
        })
      ).status === 204,
      "POST heartbeat failed",
    );
    await sleep(1100);
    privateData = await (
      await fetch(base + "/api/private", { headers: auth })
    ).json();
    assert(
      privateData.find((v) => v.id === "ha").state.status === "up",
      "heartbeat did not recover",
    );
    assert(
      (
        await fetch(base + "/api/heartbeat/ha", {
          headers: { Authorization: "Bearer " + token },
        })
      ).status === 204,
      "GET heartbeat failed",
    );
    if (process.env.PLAYWRIGHT_MODULE) {
      const { chromium } = require(process.env.PLAYWRIGHT_MODULE);
      browser = await chromium.launch({
        headless: true,
        args: ["--no-sandbox"],
        ...(process.env.CHROMIUM_PATH
          ? { executablePath: process.env.CHROMIUM_PATH }
          : {}),
      });
      const context = await browser.newContext();
      const page = await context.newPage();
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(base);
      await page.waitForFunction(
        () => document.querySelectorAll(".service").length === 1,
      );
      assert(
        (await page.locator("#summary").textContent()) ===
          "All services operational",
        "public status failed: " +
          (await page.locator("#summary").textContent()),
      );
      assert(
        !(await page.locator("body").textContent()).includes(
          "Private Home Assistant",
        ),
        "private name in page",
      );
      await page.setViewportSize({ width: 390, height: 844 });
      assert(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
        "mobile page overflows",
      );
      const owner = await browser.newContext({
        httpCredentials: { username: "owner", password },
      });
      const admin = await owner.newPage();
      admin.on("pageerror", (e) => errors.push(e.message));
      await admin.goto(base + "/dashboard");
      await admin.waitForFunction(
        () => document.querySelectorAll(".service").length === 2,
      );
      await admin
        .getByRole("button", { name: /Private Home Assistant/ })
        .click();
      assert(
        (await admin.locator("#detail h2").first().textContent()) ===
          "Private Home Assistant",
        "service selection failed",
      );
      assert(errors.length === 0, "browser errors " + errors.join(","));
      console.log(
        "Browser: public/private views, selection, mobile width and JavaScript passed",
      );
    }
    const stat = fs.readFileSync(`/proc/${proc.pid}/status`, "utf8");
    console.log(stat.match(/^VmRSS:.*$/m)?.[0]);
    console.log(
      "Smoke: HTTP checks, authenticated GET/POST heartbeats, expiry/recovery and access isolation passed",
    );
  } finally {
    if (browser) await browser.close();
    if (proc) {
      proc.kill("SIGTERM");
      await new Promise((r) => {
        if (proc.exitCode !== null) return r();
        proc.once("exit", r);
        setTimeout(() => {
          proc.kill("SIGKILL");
          r();
        }, 5000).unref();
      });
    }
    if (fixture) await new Promise((r) => fixture.close(r));
    fs.rmSync(dir, { recursive: true, force: true });
  }
})().catch((e) => {
  console.error(e.message);
  process.exitCode = 1;
});
