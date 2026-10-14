export type * from './generated/contracts.ts';
export {createAdminClient,AdminTransportError,validateContract} from './admin.ts';
export type {AdminClient,AdminOperation,AdminSuccess} from './admin.ts';

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
