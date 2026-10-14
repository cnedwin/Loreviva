import {defineConfig} from '@playwright/test';
export default defineConfig({testDir:'./tests/e2e',timeout:180000,workers:1,fullyParallel:false,reporter:'line',projects:[{name:'creator',testMatch:'creator-flow.spec.ts'},{name:'fix1-settings',testMatch:'settings-entry.spec.ts'},{name:'player',testMatch:'player-flow.spec.ts'},{name:'maintenance',testMatch:'maintenance-flow.spec.ts'}]});
