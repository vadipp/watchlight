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
    const requests = [];
    const gates = new Map();
    let failDetails = false;
    const history = Array.from({ length: 240 }, (_, i) => ({
      time: 1700000000 + i * 30 + (i >= 120 ? 300 : 0),
      ok: i !== 60, latency: 1 + i % 7,
    })).reverse();
    const monitor = {
      latency_ms: 1,
      id: 'web', name: 'Public website', type: 'http', interval_seconds: 30,
      uptime: 99.88, samples: 859,
      state: { status: 'up', checked: history[0].time, latency: 1 },
      history, incidents: [],
    };
    await page.route('http://watchlight.test/**', async route => {
      const url = new URL(route.request().url());
      const end = history[0].time+30, start = end - ({'4h':4,'24h':24,'7d':168}[url.searchParams.get('window')] || 24)*3600;
      monitor.window_start=start; monitor.window_end=end;
      monitor.buckets=Array.from({length:240},(_,i)=>{
        const time=start+(end-start)*i/240, finish=start+(end-start)*(i+1)/240;
        const points=history.filter(p=>p.time>=time && p.time<finish);
        let up=0,down=0;
        for(const p of history) {const coverage=Math.max(0,Math.min(finish,p.time+30)-Math.max(time,p.time)); if(p.ok)up+=coverage;else down+=coverage;}
        return {time,end:finish,checks:points.length,failures:points.filter(p=>!p.ok).length,up,down,latency:points.length?points.reduce((n,p)=>n+p.latency,0)/points.length:0};
      });
      if (url.pathname === '/api/health') return route.fulfill({json:{ok:true,pending_notifications:0}});
      if (url.pathname.startsWith('/api/')) {
        requests.push(url.pathname+url.search);
        const owner=url.pathname.startsWith('/api/private');
        const monitors=owner
          ? ['Operations','Websites','Services','Public'].flatMap((group,g) => Array.from({length:g===0?36:3},(_,i)=>({...monitor,id:`${g}-${i}`,name:`${group} service ${i}`,group,state:{...monitor.state,status:i===2?'down':'up'},check:{type:'http',target:'https://internal.example.test/health',description:'A local service probe',interval:'30s',timeout:'10s',failures:3,successes:1,expected_status:200}})))
          : [{...monitor,type:undefined,id:'web',name:'Public website'},{...monitor,type:undefined,id:'other',name:'Secondary website'}];
        const id=url.pathname.split('/')[3];
        if(!id) return route.fulfill({json:monitors.map(v=>({id:v.id,name:v.name,group:v.group,state:{status:v.state.status}}))});
        if(gates.has(url.searchParams.get('window'))) await gates.get(url.searchParams.get('window'));
        if(failDetails) return route.fulfill({status:503,body:'Unavailable'}).catch(()=>{});
        const detail={...monitors.find(v=>v.id===id)};delete detail.history;
        return route.fulfill({json:detail}).catch(()=>{});
      }
      const file = ['/', '/dashboard'].includes(url.pathname) ? 'index.html' : url.pathname.slice(1);
      return route.fulfill({
        body: fs.readFileSync(path.join(__dirname, '../web', file)),
        contentType: file.endsWith('.css') ? 'text/css' : file.endsWith('.js') ? 'text/javascript' : 'text/html',
      });
    });
    await page.goto('http://watchlight.test/');
    await page.waitForSelector('.bars');
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
      assert(result.gaps >= 1, `gap lost at ${width}`);
      assert.equal(result.aspect, 'none');
      console.log(`${width}px: ${result.count} blocks, minimum width ${result.min.toFixed(1)}px`);
    }
    assert.equal(await page.locator('.service-group').count(), 0, 'owner groups leaked into public UI');
    assert.equal(await page.locator('.technical').count(), 0, 'technical panel exposed publicly');
    assert.deepEqual(await page.locator('.chart-axis span').allTextContents(), ['5 ms','2.5 ms','0 ms']);
    await page.getByRole('button', {name:'4 hours',exact:true}).click();
    await page.waitForFunction(() => document.querySelector('.window-selector button').getAttribute('aria-pressed') === 'true');
    await page.locator('.bar').first().click();
    assert.match(await page.locator('.bar-details').textContent(), /unknown/);
    assert.match(await page.locator('.times').textContent(), /\d{2}\.\d{2}\.\d{4} \d{2}:\d{2}:\d{2}/);
    await page.getByRole('button', {name:'7 days',exact:true}).click();
    await page.waitForFunction(() => document.querySelector('.metric span').textContent.includes('7d'));
    assert.equal(await page.locator('.chart-grid').count(), 3);
    const listCount=()=>requests.filter(u=>u==='/api/public').length;
    const initialLists=listCount();
    await page.getByRole('button',{name:'24 hours',exact:true}).click();
    await page.waitForSelector('.metric');
    const beforeCache=requests.length;
    await page.getByRole('button',{name:'4 hours',exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('.metric span').textContent.endsWith('4h'));
    assert.equal(requests.length,beforeCache,'fresh window cache was not reused');
    assert.equal(listCount(),initialLists,'window change reloaded list');
    await page.locator('.service').filter({hasText:'Secondary website'}).click();
    await page.waitForSelector('.bars');
    const beforeMonitorCache=requests.length;
    await page.locator('.service').filter({hasText:'Public website'}).click();
    await page.waitForSelector('.bars');
    assert.equal(requests.length,beforeMonitorCache,'fresh monitor cache was not reused');
    // Hold one detail response, then switch away. Its request must be aborted.
    await page.reload();await page.waitForSelector('.bars');
    let release;gates.set('7d',new Promise(r=>release=r));
    const aborted=[];page.on('requestfailed',r=>aborted.push(r.url()));
    await page.getByRole('button',{name:'7 days',exact:true}).click();
    await page.waitForTimeout(100);
    assert.equal(await page.locator('.service').count(),2,'list disappeared during detail loading');
    await page.locator('.service').filter({hasText:'Secondary website'}).click();
    await page.getByRole('button',{name:'4 hours',exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('.metric span')?.textContent.endsWith('4h'));
    release();gates.delete('7d');await page.waitForTimeout(100);
    assert.match(await page.locator('#detail h2').first().textContent(),/Secondary/);
    assert.match(await page.locator('.metric span').first().textContent(),/4h$/);
    assert(aborted.some(u=>u.includes('window=7d')),'obsolete detail fetch was not cancelled');
    // A failed detail request must leave the list operational.
    failDetails=true;
    await page.getByRole('button',{name:'7 days',exact:true}).click();
    await page.waitForSelector('#detail .error', {state:'attached'});
    await page.waitForTimeout(100);
    assert.match(await page.locator('#detail .error').textContent(), /Unable to load details/);
    assert.equal(await page.locator('.service').count(),2);
    failDetails=false;
    await page.getByRole('button',{name:'24 hours',exact:true}).click();
    await page.waitForSelector('.bars');
    console.log('On-demand details: window cache, no list refetch, cancellation, stale-response protection and failure isolation passed');
    await page.goto('http://watchlight.test/dashboard');
    await page.waitForSelector('.service-group');
    assert.equal(await page.locator('.service-group').count(), 4);
    assert.equal(await page.locator('.service-group[open]').count(), 1);
    const websiteGroup = page.locator('.service-group').nth(1);
    await websiteGroup.locator('summary').click();
    assert.match(await websiteGroup.locator('.service').first().textContent(), /Websites service 2/);
    await websiteGroup.locator('.service').first().click();
    await page.waitForSelector('.technical');
    assert.match(await page.locator('.technical').textContent(), /https:\/\/internal.example.test\/health/);
    assert.match(await page.locator('#detail h2').first().textContent(), /Websites/);
    assert(await page.locator('.service-group').nth(1).getAttribute('open') !== null, 'group collapsed on selection');
    for (const width of [2048, 900, 390, 320]) {
      await page.setViewportSize({width,height:1200});
      await page.waitForTimeout(100);
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `owner overflow at ${width}`);
    }
    console.log('Owner groups: four groups, selection, expansion and responsive layout passed');
    // A constant zero series must retain a finite, labelled scale.
    history.forEach(p => { p.latency = 0; });
    await page.goto('http://watchlight.test/');
    await page.waitForSelector('.chart-axis');
    assert.deepEqual(await page.locator('.chart-axis span').allTextContents(), ['1 ms','0.5 ms','0 ms']);
    assert.equal(await page.locator('.chart-range').textContent(), 'Observed bucket mean range: 0 ms – 0 ms');
    // Sub-millisecond data must not round all scale labels to zero.
    history.forEach(p => { p.latency = 0.025; });
    await page.reload();
    await page.waitForSelector('.chart-axis');
    assert.deepEqual(await page.locator('.chart-axis span').allTextContents(), ['0.05 ms','0.025 ms','0 ms']);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
