import {createRouter,createWebHistory} from 'vue-router';
import WorkspaceView from './views/WorkspaceView.vue';
import WorldEditorView from './views/WorldEditorView.vue';
import ResourcesView from './views/ResourcesView.vue';
import RuntimeView from './views/RuntimeView.vue';
import SettingsView from './views/SettingsView.vue';
export const router=createRouter({history:createWebHistory('/studio/'),routes:[{path:'/',component:WorkspaceView},{path:'/editor',component:WorldEditorView},{path:'/resources',component:ResourcesView},{path:'/runtime',component:RuntimeView},{path:'/settings',component:SettingsView}]});
