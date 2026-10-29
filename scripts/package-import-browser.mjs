import {chromium,expect} from '@playwright/test';
import assert from 'node:assert/strict';
import {mkdir,writeFile} from 'node:fs/promises';
import path from 'node:path';
const base=process.env.LOREVIVA_PACKAGE_IMPORT_URL,bearer=process.env.LOREVIVA_PACKAGE_IMPORT_BEARER,archive=process.env.LOREVIVA_PACKAGE_IMPORT_ARCHIVE,output=process.env.LOREVIVA_PACKAGE_IMPORT_EVIDENCE,executable=process.env.LOREVIVA_TEST_CHROMIUM;
if(!base||!bearer||!archive||!output||!executable)throw Error('explicit owned synthetic import fixture required');
const origin=new URL(base);if(origin.protocol!=='http:'||origin.hostname!=='127.0.0.1'||!origin.port||origin.origin!==base)throw Error('owned loopback origin required');
await mkdir(output,{recursive:true});const browser=await chromium.launch({executablePath:executable,headless:true,chromiumSandbox:true});const context=await browser.newContext({viewport:{width:1440,height:1000}});const page=await context.newPage();page.setDefaultTimeout(15000);const errors=[],steps=[];
page.on('pageerror',e=>errors.push(e.message));
try{
 assert.equal((await page.goto(base+'/studio/')).status(),200);await page.getByLabel('操作员凭据').fill(bearer);await page.getByRole('button',{name:'连接',exact:true}).click();await page.getByRole('heading',{name:'工作空间',exact:true}).waitFor();await page.getByRole('link',{name:/内容与资源/}).click();
 const panel=page.getByRole('region',{name:'内容包导入预览'});await panel.getByLabel('选择内容包').setInputFiles(archive);await panel.getByRole('button',{name:'读取内容包',exact:true}).click();await panel.getByRole('heading',{name:'来源与使用范围'}).waitFor();await panel.getByLabel('我已核对来源许可，允许在本项目编辑和复用以上内容。').check();await panel.getByRole('button',{name:'检查本地匹配',exact:true}).click();await panel.getByRole('heading',{name:'本地匹配通过，尚未导入',exact:true}).waitFor();steps.push('original archive and actual local preview');
 // The owning HTTP fixture corrupts a real committed success. Let Chromium
 // upload the original File directly: request interception omits upload bytes.
 const completed=page.waitForResponse(r=>r.url().includes('/content-packages/imports?')&&r.request().method()==='POST');
 await panel.getByRole('button',{name:'导入为隔离草稿',exact:true}).click();assert.equal((await completed).status(),201);await expect(panel.getByRole('button',{name:'查询原导入结果',exact:true})).toBeEnabled();await expect(panel.getByRole('button',{name:'导入为隔离草稿',exact:true})).toHaveCount(0);steps.push('unreadable committed success retains original request');
 await panel.getByRole('button',{name:'查询原导入结果',exact:true}).click();await panel.getByRole('heading',{name:'隔离草稿已保存',exact:true}).waitFor();steps.push('original receipt recovers a real committed import');
 const key=await panel.getByLabel('原导入请求标识',{exact:true}).inputValue();assert.ok(key);await page.screenshot({path:path.join(output,'isolated-import.png'),fullPage:true});
 await panel.getByRole('button',{name:'打开隔离草稿',exact:true}).click();await expect(page).toHaveURL(/\/editor$/);steps.push('persisted isolated draft opens in editor');
 await page.getByRole('link',{name:/内容与资源/}).click();const fresh=page.getByRole('region',{name:'内容包导入预览'});await fresh.getByLabel('原导入请求标识',{exact:true}).fill(key);await fresh.getByRole('button',{name:'读取已提交导入',exact:true}).click();await fresh.getByRole('heading',{name:'隔离草稿已保存',exact:true}).waitFor();steps.push('fresh page recovers by original key without archive');assert.deepEqual(errors,[]);
 await writeFile(path.join(output,'result.json'),JSON.stringify({passed:true,steps,errors},null,2)+'\n');console.log(JSON.stringify({passed:true,steps:steps.length,errors}));
}finally{await context.close();await browser.close()}
