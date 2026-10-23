import {defineConfig} from 'vitest/config';
import vue from '@vitejs/plugin-vue';
// Keep CSS configuration in this fingerprinted file; do not search ancestor projects.
export default defineConfig({base:'/studio/',plugins:[vue()],css:{postcss:{plugins:[]}},test:{environment:'jsdom',include:['tests/**/*.spec.ts']},build:{sourcemap:false}});
