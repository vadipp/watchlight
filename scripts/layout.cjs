// Run with PLAYWRIGHT_MODULE pointing to an installed playwright-core.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright-core');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const history = Array.from({ length: 240 }, (_, i) => ({
      time: 1700000000 + i * 30 + (i >= 120 ? 300 : 0),
      ok: i !== 60, latency: 1 + i % 7,
    })).reverse();
    const monitor = {
      id: 'web', name: 'Public website', type: 'http', interval_seconds: 30,
      uptime: 99.88, samples: 859,
      state: { status: 'up', checked: history[0].time, latency: 1 },
      history, incidents: [],
    };
    await page.route('http://watchlight.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/api/public')
        return route.fulfill({ json: [monitor] });
      const file = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
      return route.fulfill({
        body: fs.readFileSync(path.join(__dirname, '../web', file)),
        contentType: file.endsWith('.css') ? 'text/css' : file.endsWith('.js') ? 'text/javascript' : 'text/html',
      });
    });
    await page.goto('http://watchlight.test/');
    for (const width of [2048, 1200, 900, 751, 750, 390, 320, 2048]) {
      await page.setViewportSize({ width, height: 1200 });
      await page.waitForTimeout(100);
      const result = await page.evaluate(() => {
        const bars = document.querySelector('.bars');
        const blocks = [...bars.children];
        const detail = document.querySelector('#detail').getBoundingClientRect();
        return {
          overflow: document.documentElement.scrollWidth > innerWidth,
          count: blocks.length,
          min: Math.min(...blocks.map(b => b.getBoundingClientRect().width)),
          right: bars.getBoundingClientRect().right,
          detailRight: detail.right,
          failures: bars.querySelectorAll('.down').length,
          gaps: bars.querySelectorAll('.unknown').length,
          aspect: document.querySelector('svg').getAttribute('preserveAspectRatio'),
        };
      });
      assert.equal(result.overflow, false, `page overflow at ${width}`);
      assert(result.right <= result.detailRight, `history overflow at ${width}`);
      assert(result.count <= 60 && result.min >= 8, `dense bars at ${width}: ${JSON.stringify(result)}`);
      assert.equal(result.failures, 1, `failure lost at ${width}`);
      assert.equal(result.gaps, 1, `gap lost at ${width}`);
      assert.equal(result.aspect, 'none');
      console.log(`${width}px: ${result.count} blocks, minimum width ${result.min.toFixed(1)}px`);
    }
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
