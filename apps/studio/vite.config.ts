import {defineConfig} from 'vitest/config';
import vue from '@vitejs/plugin-vue';
export default defineConfig({base:'/studio/',plugins:[vue()],test:{environment:'jsdom',include:['tests/**/*.spec.ts']},build:{sourcemap:false}});
