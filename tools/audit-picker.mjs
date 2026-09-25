// 双端 UI 审计：桌面 1400×900 与手机 390×844 各跑一遍，报告横向溢出与报错。
// 用法: node tools/audit-ui.mjs http://127.0.0.1:15124/app/poolgate/
// playwright 解析：优先 PW 环境变量（项目里没有 node_modules 时用），否则按常规包名加载。
// 例：PW=/path/to/node_modules/playwright/index.mjs node tools/audit-ui.mjs http://127.0.0.1:5014/
const PW = process.env.PW || "playwright";
const { chromium } = await import(PW);
const b=await chromium.launch({executablePath:process.env.CHROME || "/usr/bin/chromium",args:["--no-sandbox"]});
const url=process.argv[2]; const out={};
for(const [name,w,h] of [["desktop",1400,900],["mobile",390,844]]){
  const p=await b.newPage({viewport:{width:w,height:h}});
  const errs=[];
  p.on("pageerror",e=>errs.push(e.message));
  await p.goto(url,{waitUntil:"networkidle"});
  const ins=p.locator("input[type=password]");
  if(await ins.count()>1){await ins.nth(0).fill("pg-test-1234");await ins.nth(1).fill("pg-test-1234");await p.getByRole("button",{name:"创建并进入"}).click();}
  else {await ins.first().fill("pg-test-1234");await p.getByRole("button",{name:"登录"}).click();}
  await p.waitForTimeout(2500);
  await p.locator(".nav a",{hasText:"账号"}).first().click();
  await p.waitForTimeout(700);
  await p.getByRole("button",{name:/添加账号/}).click();
  await p.waitForTimeout(1000);
  const m=await p.evaluate(()=>{
    const card=document.querySelector(".auth-card");
    const grid=document.querySelector(".pick-grid");
    return {
      winW:window.innerWidth,
      cardW:card?Math.round(card.getBoundingClientRect().width):null,
      cols:grid?getComputedStyle(grid).gridTemplateColumns.split(" ").length:null,
      items:document.querySelectorAll(".pick-item").length,
      authBtns:document.querySelectorAll(".pick-item button.primary").length,
      pageScrollable:document.documentElement.scrollHeight>window.innerHeight,
      bodyOverflowX:document.documentElement.scrollWidth>window.innerWidth+1,
    };
  });
  await p.screenshot({path:`/tmp/pgv/pick2-${name}.png`,fullPage:true});
  out[name]={...m,errs};
  await p.close();
}
await b.close();
console.log(JSON.stringify(out,null,2));
