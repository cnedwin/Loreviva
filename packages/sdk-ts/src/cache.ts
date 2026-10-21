/** Local invalidation coordinates disposal; it never grants platform authority. */
export type ObservableIdentity=Readonly<{loginGeneration:string;customer:string;player:string;world:string;profile:string;sessionIdentity:string;viewEpoch?:string;privacyEpoch?:string}>;
export interface ViewToken {readonly generation:number}
export class ViewOwner {
 #generation=0;#identity:ObservableIdentity|null=null;#tokens=new WeakSet<object>();#disposers=new Set<()=>void>();#closed=false;
 get identity(){return this.#identity===null?null:Object.freeze({...this.#identity})}
 enter(identity:ObservableIdentity):ViewToken{if(this.#closed)throw Error('View owner closed');this.invalidate();this.#identity=Object.freeze({...identity});return this.capture()}
 capture():ViewToken{if(this.#closed||!this.#identity)throw Error('No current view');const t=Object.freeze({generation:this.#generation});this.#tokens.add(t);return t}
 current(t:ViewToken):boolean{return !this.#closed&&this.#identity!==null&&this.#tokens.has(t)&&t.generation===this.#generation}
 mayReuseBody(t:ViewToken):boolean{return this.current(t)&&this.#identity?.viewEpoch!==undefined&&this.#identity?.privacyEpoch!==undefined}
 register(t:ViewToken,dispose:()=>void):()=>void{if(!this.current(t)){dispose();throw Error('View changed')}this.#disposers.add(dispose);return ()=>{this.#disposers.delete(dispose)}}
 invalidate(){this.#generation++;this.#identity=null;const all=[...this.#disposers];this.#disposers.clear();for(const dispose of all){try{dispose()}catch{/* Disposal of one resource must not leave the others alive. */}}}
 close(){if(this.#closed)return;this.#closed=true;this.invalidate()}
}
export function compareWireRevision(a:string,b:string):number{if(!/^(0|[1-9][0-9]*)$/.test(a)||!/^(0|[1-9][0-9]*)$/.test(b)||BigInt(a)>18446744073709551615n||BigInt(b)>18446744073709551615n)throw Error('Invalid unsigned revision');return BigInt(a)<BigInt(b)?-1:BigInt(a)>BigInt(b)?1:0}
