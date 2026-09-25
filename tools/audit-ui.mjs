// 双端 UI 审计：桌面 1400×900 与手机 390×844 各跑一遍，报告横向溢出与报错。
// 用法: node tools/audit-ui.mjs http://127.0.0.1:15124/app/poolgate/
// playwright 解析：优先 PW 环境变量（项目里没有 node_modules 时用），否则按常规包名加载。
// 例：PW=/path/to/node_modules/playwright/index.mjs node tools/audit-ui.mjs http://127.0.0.1:5014/
const PW = process.env.PW || "playwright";
const { chromium } = await import(PW);
const b=await chromium.launch({executablePath:process.env.CHROME || "/usr/bin/chromium",args:["--no-sandbox"]});
const url=process.argv[2]; const rows=[];
for(const [name,w,h] of [["桌面",1400,900],["手机",390,844]]){
  const p=await b.newPage({viewport:{width:w,height:h}});
  const errs=[];
  p.on("pageerror",e=>errs.push(e.message));
  await p.goto(url,{waitUntil:"networkidle"});
  const ins=p.locator("input[type=password]");
  if(await ins.count()>1){await ins.nth(0).fill("pg-test-1234");await ins.nth(1).fill("pg-test-1234");await p.getByRole("button",{name:"创建并进入"}).click();}
  else {await ins.first().fill("pg-test-1234");await p.getByRole("button",{name:"登录"}).click();}
  await p.waitForTimeout(2500);
  for(const [k,label] of [["overview","总览"],["accounts","账号"],["models","模型与费率"],["benchmark","测速与体检"],["logs","日志"],["settings","设置"]]){
    await p.locator(".nav a",{hasText:label}).first().click({timeout:5000});
    await p.waitForTimeout(800);
    const m=await p.evaluate(()=>{
      const de=document.documentElement;
      // 找出比视口宽的表格（应被卡片内滚动容器兜住）
      // 宽表本身没问题 —— 原型要求「宽表格在卡片内横向滚动」。
      // 只有当表格既比容器宽、又没有任何祖先能横向滚动时，才算真问题。
      let wide=0;
      const canScroll=el=>{
        for(let n=el;n&&n!==document.body;n=n.parentElement){
          const ox=getComputedStyle(n).overflowX;
          if(ox==='auto'||ox==='scroll')return true;
        }
        return false;
      };
      document.querySelectorAll("table").forEach(t=>{
        if(t.scrollWidth>t.clientWidth+2 && !canScroll(t)) wide++;
      });
      return {
        overflowX: de.scrollWidth>window.innerWidth+1,
        wideTables: wide,
        scrollH: de.scrollHeight,
        innerH: window.innerHeight,
      };
    });
    rows.push({端:name,屏:k,...m});
  }
  // 授权覆盖层（「＋ 添加账号」→ 选渠道 → 授权）也要过一遍：
  // 它是用户真会看到的界面，而且是「二维码 + 长授权地址」这种最容易撑破窄屏的一屏。
  await p.locator(".nav a",{hasText:"账号"}).first().click({timeout:5000});
  await p.waitForTimeout(400);
  const addBtn=p.locator("button",{hasText:"添加账号"});
  if(await addBtn.count()){
    await addBtn.click();
    await p.waitForTimeout(600);
    const pick=p.locator(".pick-item").first();
    if(await pick.count()){
      const authBtn=pick.locator("button").first();
      if(await authBtn.count()){
        await authBtn.click();
        await p.waitForTimeout(2500);
        const m2=await p.evaluate(()=>{
          const vw=window.innerWidth, bad=[];
          // 与正式检查同一把尺子：宽内容只要有能横滚的祖先，就是「卡片内滚动」，
          // 按原型是允许的；只有真把页面撑出横向滚动才算问题。
          const canScroll=el=>{
            for(let n=el;n&&n!==document.body;n=n.parentElement){
              const ox=getComputedStyle(n).overflowX;
              if(ox==='auto'||ox==='scroll')return true;
            }
            return false;
          };
          document.querySelectorAll("*").forEach(el=>{
            const r=el.getBoundingClientRect();
            if(r.width>vw+1&&!canScroll(el)) bad.push(el.tagName+"."+(el.className||"").toString().slice(0,24));
          });
          return {overflowX:document.documentElement.scrollWidth>vw+1, worst:bad.slice(0,3)};
        });
        rows.push({端:name,屏:"授权覆盖层",...m2});
        await p.screenshot({path:`/tmp/audit-ui-auth-${name}.png`,fullPage:true});
        const cancel=p.locator("button",{hasText:"取消授权"});
        if(await cancel.count())await cancel.click();
      }
    }
    await p.waitForTimeout(300);
  }
  if(errs.length) rows.push({端:name,屏:"(报错)",errs:[...new Set(errs)]});
  await p.close();
}
await b.close();
const bad=rows.filter(r=>r.overflowX||r.wideTables||r.errs||r.worst?.length);
console.log("问题项：",bad.length);
for(const r of bad) console.log(" ",JSON.stringify(r));
console.log("\n全部：");
for(const r of rows) console.log(`  ${r.端} ${String(r.屏).padEnd(12)} 横向溢出=${r.overflowX?'有':'无'} 裸宽表=${r.wideTables||0} 页高=${r.scrollH}`);
