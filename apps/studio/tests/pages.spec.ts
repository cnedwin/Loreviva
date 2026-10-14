import {expect,it,vi} from 'vitest';
import {collectPages} from '../src/lib/pages';
it('empty authorized pages still follow the opaque cursor and failures do not become empty results',async()=>{
 const read=vi.fn().mockResolvedValueOnce({items:[],next_cursor:'opaque'}).mockResolvedValueOnce({items:['registered']});expect(await collectPages(read)).toEqual(['registered']);expect(read.mock.calls).toEqual([[undefined],['opaque']]);
 await expect(collectPages(vi.fn().mockRejectedValue(Error('revoked')))).rejects.toThrow('revoked');
 await expect(collectPages(vi.fn().mockResolvedValue({items:[],next_cursor:'same'}))).rejects.toThrow('分页游标未前进');
});
