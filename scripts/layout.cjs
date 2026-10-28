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
      if (url.pathname === '/api/health') return route.fulfill({json:{ok:true,pending_notifications:0}});
      if (url.pathname === '/api/private') return route.fulfill({json:
        ['Operations','Websites','Services','Public'].flatMap((group,g) => Array.from({length: g === 0 ? 36 : 3}, (_,i) => ({...monitor, id:`${g}-${i}`,name:`${group} service ${i}`,group, state:{...monitor.state,status:i===2?'down':'up'}, check:{type:'http',target:'https://internal.example.test/health',description:'A local service probe',interval:'30s',timeout:'10s',failures:3,successes:1,expected_status:200}})))
      });
      if (url.pathname === '/api/public')
        return route.fulfill({ json: [{...monitor,type:undefined,interval_seconds:undefined,latency_ms:1}] });
      const file = ['/', '/dashboard'].includes(url.pathname) ? 'index.html' : url.pathname.slice(1);
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
    assert.equal(await page.locator('.service-group').count(), 0, 'owner groups leaked into public UI');
    assert.equal(await page.locator('.technical').count(), 0, 'technical panel exposed publicly');
    await page.goto('http://watchlight.test/dashboard');
    await page.waitForSelector('.service-group');
    assert.equal(await page.locator('.service-group').count(), 4);
    assert.equal(await page.locator('.service-group[open]').count(), 1);
    const websiteGroup = page.locator('.service-group').nth(1);
    await websiteGroup.locator('summary').click();
    assert.match(await websiteGroup.locator('.service').first().textContent(), /Websites service 2/);
    await websiteGroup.locator('.service').first().click();
    assert.match(await page.locator('.technical').textContent(), /https:\/\/internal.example.test\/health/);
    assert.match(await page.locator('#detail h2').first().textContent(), /Websites/);
    assert(await page.locator('.service-group').nth(1).getAttribute('open') !== null, 'group collapsed on selection');
    for (const width of [2048, 900, 390, 320]) {
      await page.setViewportSize({width,height:1200});
      await page.waitForTimeout(100);
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `owner overflow at ${width}`);
    }
    console.log('Owner groups: four groups, selection, expansion and responsive layout passed');
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
