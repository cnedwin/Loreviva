import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import path from 'node:path';
import {verifyStudio} from './studio-artifact.mjs';

const repo=fileURLToPath(new URL('../',import.meta.url));
// A plain Go compilation does not execute frontend freshness checks.
await verifyStudio(repo,path.join(repo,'internal/app/studio_dist'));
const result=spawnSync(process.env.GO_EXE||'go',['build',...process.argv.slice(2)],{cwd:repo,stdio:'inherit',shell:false});
if(result.error)throw result.error;
if(result.signal||result.status!==0)process.exit(result.status||1);
