import type {TextSegment,MemoryGovernanceRequest,TurnRequest} from '../../packages/sdk-ts/src/generated/contracts.ts';

const dialogue: TextSegment={segment_id:'s',speaker_actor_id:'actor',kind:'DIALOGUE',text:'hello',committed_event_ids:[]};
const text: string=dialogue.text;
const result: TextSegment={...dialogue,kind:'OPERATION_RESULT',committed_event_ids:['event']};
const correction: MemoryGovernanceRequest={operation:'CORRECT',memory_ids:['memory'],expected_revisions:{memory:'1'},replacement:'corrected'};
const deletion: MemoryGovernanceRequest={operation:'DELETE',memory_ids:['memory'],expected_revisions:{memory:'1'}};
const revocation: MemoryGovernanceRequest={...deletion,operation:'REVOKE'};
const textInput: TurnRequest['input']={kind:'TEXT',text:'hello'};
const actionInput: TurnRequest['input']={kind:'ACTION',action_type:'borrow',arguments:{}};
const choiceInput: TurnRequest['input']={kind:'CHOICE',choice_id:'choice'};

// @ts-expect-error TextSegment must retain object shape.
const primitiveSegment: TextSegment=42;
// @ts-expect-error Required sibling text cannot disappear behind allOf.
const missingBase: TextSegment={segment_id:'s',speaker_actor_id:'actor',kind:'DIALOGUE',committed_event_ids:[]};
// @ts-expect-error Operation results require a nonempty event list.
const missingEvent: TextSegment={...dialogue,kind:'OPERATION_RESULT',committed_event_ids:[]};
// @ts-expect-error Governance must retain object shape.
const primitiveGovernance: MemoryGovernanceRequest=42;
// @ts-expect-error Governance requires its baseline revisions.
const missingRevisions: MemoryGovernanceRequest={operation:'DELETE',memory_ids:['memory']};
// @ts-expect-error Correction requires replacement.
const missingReplacement: MemoryGovernanceRequest={operation:'CORRECT',memory_ids:['memory'],expected_revisions:{memory:'1'}};
// @ts-expect-error Correction is limited to one memory ID.
const tooManyCorrections: MemoryGovernanceRequest={operation:'CORRECT',memory_ids:['a','b'],expected_revisions:{a:'1',b:'1'},replacement:'corrected'};
// @ts-expect-error DELETE forbids replacement.
const deletionReplacement: MemoryGovernanceRequest={operation:'DELETE',memory_ids:['memory'],expected_revisions:{memory:'1'},replacement:'wrong'};
// @ts-expect-error REVOKE forbids replacement.
const revocationReplacement: MemoryGovernanceRequest={operation:'REVOKE',memory_ids:['memory'],expected_revisions:{memory:'1'},replacement:'wrong'};
// @ts-expect-error Input must retain object shape.
const primitiveInput: TurnRequest['input']=42;
// @ts-expect-error TEXT requires text.
const missingText: TurnRequest['input']={kind:'TEXT'};
// @ts-expect-error ACTION requires action_type.
const missingActionType: TurnRequest['input']={kind:'ACTION',arguments:{}};
// @ts-expect-error ACTION requires arguments.
const missingArguments: TurnRequest['input']={kind:'ACTION',action_type:'borrow'};
// @ts-expect-error CHOICE requires choice_id.
const missingChoice: TurnRequest['input']={kind:'CHOICE'};
void [text,result,correction,deletion,revocation,textInput,actionInput,choiceInput];

// Both the unchanged default and the explicit source-backed opt-in compile.
import type {Operations,ProjectionResponses,MemoryGovernanceCommandV1} from '../../packages/sdk-ts/src/generated/contracts.ts';
declare const oldMemory: Operations['getMemory']['responses']['200'];
const oldMemoryID: string=oldMemory.memory_id;
declare const oldPage: Operations['listMemories']['responses']['200'];
const oldFirstID: string|undefined=oldPage.memories[0]?.memory_id;
const newInput: Operations['getMemory']['input']={path:{instance_id:'world',memory_id:'memory'},query:{include:'provenance_v1'}};
declare const projectedMemory: ProjectionResponses['getMemory']['provenance_v1'];
const projectedID: string=projectedMemory.memory.memory_id;
const selectorDelete: MemoryGovernanceCommandV1={operation:'DELETE',selector:{kind:'IDS',target_kind:'SOURCE',ids:['source'],expected_revisions:{source:'1'}}};
const oldGovernanceStillAccepted: MemoryGovernanceCommandV1=deletion;
void [oldMemoryID,oldFirstID,newInput,projectedID,selectorDelete,oldGovernanceStillAccepted];
