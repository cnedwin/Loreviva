import {test} from '@playwright/test';
test('actual settings-only App entry',async()=>{await import('../../scripts/studio-settings-browser.mjs')});
