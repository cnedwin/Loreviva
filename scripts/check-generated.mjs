import {mkdtemp,readFile,readdir,rm} from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {fileURLToPath} from 'node:url';
import {generateContracts,canonicalSourceRoot} from './generate-contracts.mjs';
const repo=fileURLToPath(new URL('../',import.meta.url));
async function inventory(dir,base=dir){try {const entries=await readdir(dir,{withFileTypes:true});const all=await Promise.all(entries.map(e=>e.isDirectory()?inventory(path.join(dir,e.name),base):[path.relative(base,path.join(dir,e.name)).replaceAll('\\','/')]));return all.flat()}catch(err){if(err.code==='ENOENT')return [];throw err}}
export async function checkGenerated({sourceRoot=canonicalSourceRoot,outputRoot=repo}={}) {
 const temporary=await mkdtemp(path.join(os.tmpdir(),'loreviva-generated-check-'));
 try {
  const result=await generateContracts({sourceRoot,outputRoot:temporary});const differences=[];
  for(const output of result.outputs){try {if(!(await readFile(path.join(outputRoot,output))).equals(await readFile(path.join(temporary,output))))differences.push(output)}catch(err){if(err.code==='ENOENT')differences.push(output);else throw err}}
  for(const root of ['contracts/generated','internal/generated','packages/sdk-ts/src/generated']) for(const output of await inventory(path.join(outputRoot,root))) {const relative=root+'/'+output;if(!result.outputs.includes(relative))differences.push(relative)}
  return [...new Set(differences)].sort();
 }finally {await rm(temporary,{recursive:true,force:true})}
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url)){const differences=await checkGenerated();if(differences.length){console.error('Generated contract drift:\n'+differences.join('\n'));process.exitCode=1}else console.log('Generated contracts: zero diff')}
