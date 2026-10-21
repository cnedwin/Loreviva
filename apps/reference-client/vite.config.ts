import {defineConfig} from 'vitest/config';
import vue from '@vitejs/plugin-vue';
export default defineConfig({
 plugins:[vue()],
 test:{projects:[
  {extends:true,test:{name:'reference-ui',environment:'jsdom',include:['src/**/*.test.ts']}},
  {extends:true,test:{name:'integration-node',environment:'node',include:['../../tests/sdk/**/*.test.ts','../../examples/thin-backend/*.test.ts'],testTimeout:300000,hookTimeout:300000}}
 ]}
});
