// Each page remains a fresh server-authorized read. Empty pages can have a cursor.
export async function collectPages<T>(read:(cursor?:string)=>Promise<{items:T[];next_cursor?:string}>):Promise<T[]>{
 const items:T[]=[];const seen=new Set<string>();let cursor:string|undefined;
 do{const page=await read(cursor);items.push(...page.items);cursor=page.next_cursor;if(cursor&&seen.has(cursor))throw Error('分页游标未前进');if(cursor)seen.add(cursor)}while(cursor);
 return items;
}
