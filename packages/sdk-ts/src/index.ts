export type * from './generated/contracts.ts';
export {createAdminClient,AdminTransportError,validateContract} from './admin.ts';
export type {AdminClient,AdminOperation,AdminSuccess} from './admin.ts';
export {createIntegrationTransport,createPreparedIntegrationOwner,createReferenceBffClient,IntegrationResponseError,BffResponseError} from './client.ts';
export type {Profile,Operation,CredentialRole,Input,TransportResult,TransportOptions,IntegrationTransport,OperationsForRole,PreparedIntegrationRequest,PreparedRequestSnapshot} from './client.ts';
export type {BrowserBffAdapter,BffSessionView,BffOriginalObservation,BffCustomerResult,ClientGetOperation,DurableOperation} from './client.ts';
export {RecoveryController,BffRecoveryController} from './recovery.ts';
export type {OriginalTurnReference,OriginalTurnQueryPort,OriginalObservation,BffOriginalReference,BffOriginalQueryPort,BffRecoveryObservation} from './recovery.ts';
export {ViewOwner,compareWireRevision} from './cache.ts';
export type {ViewToken,ObservableIdentity} from './cache.ts';
export {MediaController} from './media.ts';
export type {AuthorizedMediaBody,RuntimeMediaPort,MediaSink} from './media.ts';
export type {SSEFrame,AsyncDisposableEventStream} from './sse.ts';

export interface LorevivaClientConfig {
  baseUrl: string;
  sessionToken: string;
}

// The wrapper contains client configuration only. Public DTOs are generated.
export function createLorevivaClient(options: LorevivaClientConfig): Readonly<LorevivaClientConfig> {
  const url = new URL(options.baseUrl);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) {
    throw new Error('baseUrl requires HTTP(S) without embedded credentials');
  }
  if (!options.sessionToken) throw new Error('sessionToken is required');
  return Object.freeze({ baseUrl: url.toString().replace(/\/$/, ''), sessionToken: options.sessionToken });
}
