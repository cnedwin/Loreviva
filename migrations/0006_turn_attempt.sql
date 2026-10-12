-- Task6 bounded Mock quote/envelope. Further Task6 tables append here only.
CREATE TABLE IF NOT EXISTS harness_quotes (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 quote_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NULL, project_id VARBINARY(64) NOT NULL,
 document JSON NOT NULL, created_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 credit_unit VARCHAR(16) NOT NULL DEFAULT 'WE_CREDIT',
 cost_unit VARCHAR(16) NOT NULL DEFAULT 'MONEY', currency VARCHAR(3) NOT NULL DEFAULT 'USD',
 rounding VARCHAR(24) NOT NULL DEFAULT 'SUM_THEN_HALF_EVEN',
 PRIMARY KEY(tenant_id,product_id,quote_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 CHECK(credit_unit='WE_CREDIT' AND cost_unit='MONEY' AND currency='USD' AND rounding='SUM_THEN_HALF_EVEN'),
 CHECK(expires_at=DATE_ADD(created_at,INTERVAL 60 SECOND))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_envelopes (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, quote_id VARBINARY(64) NOT NULL,
 document JSON NOT NULL, accepted_at DATETIME(6) NOT NULL,
 execute_by DATETIME(6) NOT NULL, settle_by DATETIME(6) NOT NULL,
 max_calls INT NOT NULL, max_steps INT NOT NULL,
 calls_allocated INT NOT NULL DEFAULT 0, steps_used INT NOT NULL DEFAULT 0,
 supplier_cap BIGINT NOT NULL, supplier_allocated BIGINT NOT NULL DEFAULT 0,
 state VARCHAR(24) NOT NULL DEFAULT 'ACTIVE', narrative_generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY harness_quote_consumed(tenant_id,product_id,quote_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,quote_id) REFERENCES harness_quotes(tenant_id,product_id,quote_id),
 CHECK(max_calls BETWEEN 2 AND 40 AND max_steps BETWEEN 20 AND 402),
 CHECK(calls_allocated BETWEEN 0 AND max_calls AND steps_used BETWEEN 0 AND max_steps),
 CHECK(supplier_cap>=0 AND supplier_allocated BETWEEN 0 AND supplier_cap),
 CHECK(state IN ('ACTIVE','COMMITTED','CUTOFF','ACCOUNTING_ONLY','CANCELLED')),
 CHECK(narrative_generation>0 AND settle_by>execute_by AND execute_by>accepted_at)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_attempt_allocations (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 supplier_atoms BIGINT NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id),
 CHECK(supplier_atoms>=0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_context_bindings (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS r0_generation_consent (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 subject_id VARBINARY(64) NOT NULL, credential_id VARBINARY(64) NOT NULL,
 policy_id VARCHAR(64) NOT NULL, opted_in BOOLEAN NOT NULL,
 accepted_at DATETIME(6) NOT NULL, expires_at DATETIME(6) NULL,
 PRIMARY KEY(tenant_id,product_id,subject_id),
 FOREIGN KEY(tenant_id,product_id,subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 FOREIGN KEY(credential_id) REFERENCES operator_credentials(credential_id),
 CHECK(policy_id='R0_OWNER_PRIVATE_GENERATION_30D'),
 CHECK((opted_in=0 AND expires_at IS NULL) OR (opted_in=1 AND expires_at=DATE_ADD(accepted_at,INTERVAL 30 DAY)))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_turn_quotes (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 quote_id VARBINARY(64) NOT NULL, actor_id VARBINARY(64) NOT NULL,
 input_bound BIGINT UNSIGNED NOT NULL, output_bound BIGINT UNSIGNED NOT NULL,
 public_document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,quote_id),
 FOREIGN KEY(tenant_id,product_id,quote_id) REFERENCES harness_quotes(tenant_id,product_id,quote_id),
 CHECK(input_bound BETWEEN 1 AND 16000 AND output_bound BETWEEN 1 AND 4000)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_turns (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 original_key VARBINARY(255) NOT NULL, command_hash BINARY(32) NOT NULL,
 quote_id VARBINARY(64) NOT NULL, actor_id VARBINARY(64) NOT NULL,
 input_source_id VARBINARY(64) NOT NULL, input_hash BINARY(32) NOT NULL,
 versions JSON NOT NULL, accepted_commit_key VARBINARY(255) NOT NULL,
 fsm_state VARCHAR(32) NOT NULL DEFAULT 'ACCEPTED',
 PRIMARY KEY(tenant_id,product_id,task_id),
 UNIQUE KEY original_turn(tenant_id,product_id,world_instance_id,owner_subject_id,original_key),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,quote_id) REFERENCES harness_turn_quotes(tenant_id,product_id,quote_id),
 CHECK(fsm_state IN ('ACCEPTED','CONTEXT_BOUND','AUTHORIZED','SEND_MARKED','PROVIDER_UNKNOWN','RESULT_RECEIVED','RESULT_KNOWN','VALIDATED','EXTRA_APPROVED','LOCAL_APPROVAL_READY','LOCAL_COMMIT_READY','APPROVED','COMMITTED','REJECTED','CANCELLED','ACCOUNTING_ONLY'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_turn_steps (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 step_number INT NOT NULL, from_state VARCHAR(32) NOT NULL, to_state VARCHAR(32) NOT NULL,
 fence BIGINT UNSIGNED NOT NULL, occurred_at DATETIME(6) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,step_number),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id),
 CHECK(step_number BETWEEN 1 AND 20 AND fence>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_query_services (
 installation_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL,
 action VARBINARY(64) NOT NULL, policy VARBINARY(64) NOT NULL, enabled BOOLEAN NOT NULL,
 PRIMARY KEY(installation_id,service_id),
 CHECK(service_id='r0-mock-original-query' AND action='attempt.cost.query' AND policy='R0_ORIGINAL_MOCK_COST_V1')
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_original_queries (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 installation_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL, policy VARBINARY(64) NOT NULL,
 marker_key VARBINARY(128) NOT NULL, request_identity JSON NOT NULL,
 fence BIGINT UNSIGNED NOT NULL DEFAULT 0, lease_owner VARBINARY(64) NULL, lease_expires_at DATETIME(6) NULL,
 generation BIGINT UNSIGNED NOT NULL, queries_used INT NOT NULL DEFAULT 0,
 cost_queries_used INT NOT NULL DEFAULT 0, next_cost_query_at DATETIME(6) NULL,
 next_query_at DATETIME(6) NOT NULL, provider_state VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN',
 metadata JSON NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(installation_id,service_id) REFERENCES harness_query_services(installation_id,service_id),
 CHECK(cost_queries_used BETWEEN 0 AND 3 AND queries_used BETWEEN 0 AND 3 AND provider_state IN ('UNKNOWN','SUCCEEDED'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_result_sources (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 storage_key VARBINARY(64) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,storage_key),
 UNIQUE KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_attempt_contexts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_original_validations (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 body_hash CHAR(64) NOT NULL, metadata JSON NOT NULL,
 validation_code VARCHAR(32) NOT NULL, validator_version VARBINARY(64) NOT NULL,
 receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 CHECK(validation_code IN ('STRUCTURE_VALID','STRUCTURE_INVALID','REFUSAL'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_candidate_approvals (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 document JSON NOT NULL, receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_turn_commits (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 result_id VARBINARY(64) NOT NULL, receipt_key VARBINARY(128) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,result_id) REFERENCES approved_results(tenant_id,product_id,record_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_service_lines (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 service_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL, quote_id VARBINARY(64) NOT NULL,
 input_units BIGINT UNSIGNED NOT NULL, output_units BIGINT UNSIGNED NOT NULL,
 service_atoms BIGINT NOT NULL, service_unit VARCHAR(16) NOT NULL,
 supplier_atoms BIGINT NOT NULL, supplier_unit VARCHAR(16) NOT NULL, supplier_currency VARCHAR(16) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,service_id) REFERENCES service_events(tenant_id,product_id,record_id),
 CHECK(service_atoms=0 AND supplier_atoms=0 AND service_unit='WE_CREDIT' AND supplier_unit='MONEY' AND supplier_currency='syntheticUSD')
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_local_rounds (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 phase VARCHAR(16) NOT NULL, round_number BIGINT UNSIGNED NOT NULL,
 state VARCHAR(16) NOT NULL, reason VARCHAR(32) NOT NULL,
 body_hash CHAR(64) NOT NULL, original_receipt_key VARBINARY(128) NOT NULL,
 conflict_document JSON NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id,phase,round_number),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 CHECK(phase IN ('APPROVAL','WORLD_COMMIT')),
 CHECK(state IN ('PENDING','COMMITTED','ROLLED_BACK')),
 CHECK(reason IN ('','LOCAL_FAILURE','RESOURCE_CONFLICT')),
 CHECK(round_number BETWEEN 1 AND 20)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_recovery_approvals (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 original_attempt_id VARBINARY(64) NOT NULL, extra_attempt_id VARBINARY(64) NOT NULL,
 document JSON NOT NULL, receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id,original_attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_necessary_cost_records (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 ordinal INT NOT NULL, query_key VARBINARY(64) NOT NULL, lease_fence BIGINT UNSIGNED NOT NULL,
 began_at DATETIME(6) NOT NULL, next_query_at DATETIME(6) NOT NULL, recorded_at DATETIME(6) NULL, metadata JSON NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id,ordinal), UNIQUE KEY necessary_query_key(query_key),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES harness_original_queries(tenant_id,product_id,task_id,attempt_id),
 CHECK(ordinal BETWEEN 1 AND 3 AND lease_fence>0 AND next_query_at>began_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_original_query_records (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 kind VARCHAR(16) NOT NULL, ordinal INT NOT NULL, query_key VARBINARY(64) NOT NULL, lease_fence BIGINT UNSIGNED NOT NULL,
 began_at DATETIME(6) NOT NULL, recorded_at DATETIME(6) NULL, metadata JSON NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id,kind,ordinal), UNIQUE KEY original_query_key(query_key),
 UNIQUE KEY original_query_fence(tenant_id,product_id,task_id,attempt_id,lease_fence),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES harness_original_queries(tenant_id,product_id,task_id,attempt_id),
 CHECK(lease_fence>0 AND ((kind='REPLY' AND ordinal=0) OR (kind='FOREGROUND' AND ordinal BETWEEN 1 AND 3)))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_settlement_services (
 installation_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL,
 action VARBINARY(64) NOT NULL, policy VARBINARY(64) NOT NULL, enabled BOOLEAN NOT NULL,
 PRIMARY KEY(installation_id,service_id),
 CHECK(service_id='r0-mock-settlement' AND action='attempt.cost.settle' AND policy='R0_BOUNDED_SETTLEMENT_V1')
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_settlement_bindings (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 installation_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL, policy VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id),
 FOREIGN KEY(installation_id,service_id) REFERENCES harness_settlement_services(installation_id,service_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_cutoffs (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 risk_case_id VARBINARY(64) NOT NULL, receipt_key VARBINARY(128) NOT NULL,
 installation_id VARBINARY(64) NOT NULL, installation_generation BIGINT UNSIGNED NOT NULL,
 narrative_generation BIGINT UNSIGNED NOT NULL, settled_at DATETIME(6) NOT NULL,
 proof JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id),
 FOREIGN KEY(receipt_key) REFERENCES commit_receipts(commit_key)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_cutoff_work (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 next_run_at DATETIME(6) NOT NULL, failures BIGINT UNSIGNED NOT NULL DEFAULT 0,
 last_failure VARCHAR(16) NOT NULL DEFAULT '', done BOOLEAN NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,task_id), KEY due_cutoff(done,next_run_at),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_bill_items (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 provider VARBINARY(64) NOT NULL, provider_account VARBINARY(64) NOT NULL, bill_item_id VARBINARY(64) NOT NULL,
 item_hash BINARY(32) NOT NULL, amount BIGINT NOT NULL, document JSON NOT NULL, journal_head JSON NOT NULL,
 receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,attempt_id,provider,provider_account,bill_item_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(receipt_key) REFERENCES commit_receipts(commit_key), CHECK(amount>=0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_bill_conflicts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 provider VARBINARY(64) NOT NULL, provider_account VARBINARY(64) NOT NULL, bill_item_id VARBINARY(64) NOT NULL,
 original_hash BINARY(32) NOT NULL, conflicting_hash BINARY(32) NOT NULL, document JSON NOT NULL, journal_head JSON NOT NULL,
 receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,attempt_id,provider,provider_account,bill_item_id,conflicting_hash),
 FOREIGN KEY(receipt_key) REFERENCES commit_receipts(commit_key)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_supplier_liabilities (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 total_atoms DECIMAL(65,0) NOT NULL, debt_atoms DECIMAL(65,0) NOT NULL, final_known BOOLEAN NOT NULL, disputed BOOLEAN NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 CHECK(total_atoms>=0 AND debt_atoms>=0 AND debt_atoms<=total_atoms)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_account_blocks (
 kind VARCHAR(24) NOT NULL, scope_key VARBINARY(256) NOT NULL, account_id VARBINARY(64) NOT NULL,
 reason VARCHAR(32) NOT NULL,
 PRIMARY KEY(kind,scope_key,account_id),
 FOREIGN KEY(kind,scope_key,account_id) REFERENCES budget_accounts(kind,scope_key,account_id),
 CHECK(kind IN ('supplier_global','supplier_tenant') AND reason IN ('LATE_OVER_CAP','BILL_CONFLICT'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_risk_cases (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 risk_case_id VARBINARY(64) NOT NULL, scope_key VARBINARY(256) NOT NULL, account_id VARBINARY(64) NOT NULL, reservation_id VARBINARY(64) NOT NULL,
 original_cap BIGINT NOT NULL, cost_atoms DECIMAL(65,0) NOT NULL DEFAULT 0, unresolved BOOLEAN NOT NULL DEFAULT 1,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id), CHECK(original_cap>=0 AND cost_atoms>=0)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_bill_cursor (
 installation_id VARBINARY(64) PRIMARY KEY,
 sequence_number BIGINT UNSIGNED NOT NULL, journal_hash CHAR(64) NOT NULL,
 receipt_key VARBINARY(64) NOT NULL
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_bill_scan_receipts (
 installation_id VARBINARY(64) NOT NULL, receipt_key VARBINARY(64) NOT NULL,
 installation_generation BIGINT UNSIGNED NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(installation_id,receipt_key)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_bill_work (
 installation_id VARBINARY(64) NOT NULL, sequence_number BIGINT UNSIGNED NOT NULL,
 journal_hash CHAR(64) NOT NULL, request_identity JSON NOT NULL,
 bill_item_id VARBINARY(64) NOT NULL, item_hash CHAR(64) NOT NULL,
 next_run_at DATETIME(6) NOT NULL, failures BIGINT UNSIGNED NOT NULL DEFAULT 0,
 done BOOLEAN NOT NULL DEFAULT 0, receipt_key VARBINARY(128) NOT NULL DEFAULT '',
 PRIMARY KEY(installation_id,sequence_number), KEY due_bill(done,next_run_at,sequence_number)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_world_seals (
 tenant_id VARBINARY(191) NOT NULL, product_id VARBINARY(191) NOT NULL, world_instance_id VARBINARY(191) NOT NULL,
 installation_id VARBINARY(191) NOT NULL, installation_generation BIGINT UNSIGNED NOT NULL,
 set_hash CHAR(64) CHARACTER SET ascii NOT NULL, receipt_key VARBINARY(191) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_task_retirements (
 tenant_id VARBINARY(191) NOT NULL, product_id VARBINARY(191) NOT NULL, world_instance_id VARBINARY(191) NOT NULL, task_id VARBINARY(191) NOT NULL,
 original_generation BIGINT UNSIGNED NOT NULL, retired_generation BIGINT UNSIGNED NOT NULL, receipt_key VARBINARY(191) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id), CHECK(retired_generation=original_generation+1)
) ENGINE=InnoDB;

-- Fixed Product CANDIDATE intake without prompt or result plaintext.
CREATE TABLE IF NOT EXISTS harness_authoring_projects (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, project_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, purpose VARCHAR(32) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,project_id),
 FOREIGN KEY(tenant_id,product_id,project_id) REFERENCES world_projects(tenant_id,product_id,project_id),
 CHECK(purpose='R0_AUTHORING_PROJECT')
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_admin_candidates (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, original_key VARBINARY(128) NOT NULL, command_hash BINARY(32) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, selection JSON NOT NULL, binding JSON NOT NULL, accepted_commit_key VARBINARY(64) NOT NULL,
 fsm_state VARCHAR(32) NOT NULL DEFAULT 'ACCEPTED',
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY original_candidate(tenant_id,product_id,owner_subject_id,original_key),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,draft_id) REFERENCES content_drafts(tenant_id,product_id,draft_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_candidate_attempts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_candidate_results (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL, storage_key VARBINARY(64) NOT NULL, body_hash CHAR(64) NOT NULL, source_binding JSON NOT NULL, delete_intent BOOLEAN NOT NULL DEFAULT FALSE,
 asset_id VARBINARY(64) NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id), UNIQUE KEY candidate_storage(tenant_id,product_id,storage_key),
 KEY candidate_derived_asset(tenant_id,product_id,asset_id),
 FOREIGN KEY(tenant_id,product_id,asset_id) REFERENCES assets(tenant_id,product_id,asset_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES harness_candidate_attempts(tenant_id,product_id,task_id,attempt_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS harness_candidate_recoveries (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, original_attempt_id VARBINARY(64) NOT NULL, extra_attempt_id VARBINARY(64) NOT NULL, document JSON NOT NULL, receipt_key VARBINARY(128) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_admin_candidates(tenant_id,product_id,task_id)
);

-- Original typed candidate business decision and original joint receipt.
-- Only metadata. The actual 0/1 bytes remain governed Files references.
CREATE TABLE IF NOT EXISTS harness_candidate_commits (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 attempt_id VARBINARY(64) NOT NULL, asset_id VARBINARY(64) NOT NULL, receipt_key VARBINARY(128) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY candidate_asset(tenant_id,product_id,asset_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES harness_candidate_attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,asset_id) REFERENCES assets(tenant_id,product_id,asset_id)
) ENGINE=InnoDB;

-- Fixed Product rehearsal root. Binding contains no input or result plaintext.
CREATE TABLE IF NOT EXISTS harness_admin_rehearsals (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, original_key VARBINARY(128) NOT NULL, command_hash BINARY(32) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, binding JSON NOT NULL, accepted_commit_key VARBINARY(64) NOT NULL,
 fsm_state VARCHAR(32) NOT NULL DEFAULT 'ACCEPTED', next_input INT UNSIGNED NOT NULL DEFAULT 0,
 own_steps TINYINT UNSIGNED NOT NULL DEFAULT 0, narrative_generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY original_rehearsal(tenant_id,product_id,owner_subject_id,original_key),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,draft_id) REFERENCES content_drafts(tenant_id,product_id,draft_id),
 CHECK(next_input<=20), CHECK(own_steps<=2)
) ENGINE=InnoDB;

-- Governed explicit synthetic input source, deleted in the owning draft DELETE
-- transaction. It is not immutable evidence and must never be rebuilt on replay.
CREATE TABLE IF NOT EXISTS harness_rehearsal_input_sources (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id), KEY rehearsal_input_draft(tenant_id,product_id,draft_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_admin_rehearsals(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,draft_id) REFERENCES content_drafts(tenant_id,product_id,draft_id)
) ENGINE=InnoDB;

-- Internal version is FK-only and unavailable to ordinary published APIs.
CREATE TABLE IF NOT EXISTS harness_rehearsal_worlds (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, root_task_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, release_id VARBINARY(64) NOT NULL, initialization_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,root_task_id), UNIQUE KEY rehearsal_world(tenant_id,product_id,world_instance_id),
 UNIQUE KEY rehearsal_internal_version(tenant_id,product_id,release_id),
 FOREIGN KEY(tenant_id,product_id,root_task_id) REFERENCES harness_admin_rehearsals(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 FOREIGN KEY(tenant_id,product_id,release_id) REFERENCES releases(tenant_id,product_id,release_id)
) ENGINE=InnoDB;

-- Exact immutable server-issued child attribution. No synthetic input body.
CREATE TABLE IF NOT EXISTS harness_rehearsal_children (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, root_task_id VARBINARY(64) NOT NULL,
 input_index INT UNSIGNED NOT NULL, child_task_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NOT NULL,
 actor_id VARBINARY(64) NOT NULL, input_source_id VARBINARY(64) NOT NULL, input_hash BINARY(32) NOT NULL,
 root_generation BIGINT UNSIGNED NOT NULL, started_at DATETIME(6) NOT NULL, execute_by DATETIME(6) NOT NULL, accepted_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,root_task_id,input_index), UNIQUE KEY rehearsal_child(tenant_id,product_id,child_task_id),
 FOREIGN KEY(tenant_id,product_id,root_task_id) REFERENCES harness_admin_rehearsals(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,child_task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 CHECK(input_index<20 AND root_generation>0 AND execute_by>started_at AND execute_by<=DATE_ADD(started_at,INTERVAL 60 SECOND))
) ENGINE=InnoDB;

-- Metadata only. Report bytes and their child dependencies remain governed.
CREATE TABLE IF NOT EXISTS harness_rehearsal_reports (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 storage_key VARBINARY(64) NOT NULL, body_hash CHAR(64) NOT NULL, evidence_hash BINARY(32) NOT NULL,
 subject_privacy BIGINT UNSIGNED NOT NULL, subject_view BIGINT UNSIGNED NOT NULL,
 stage_receipt VARBINARY(64) NOT NULL, state VARCHAR(16) NOT NULL DEFAULT 'STAGED', delete_intent BOOLEAN NOT NULL DEFAULT FALSE,
 asset_id VARBINARY(64) NULL, receipt_key VARBINARY(64) NULL, commit_hash BINARY(32) NULL,
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY rehearsal_report_storage(tenant_id,product_id,storage_key),
 UNIQUE KEY rehearsal_report_asset(tenant_id,product_id,asset_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_admin_rehearsals(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,asset_id) REFERENCES assets(tenant_id,product_id,asset_id),
 CHECK(state IN ('STAGED','COMMITTED'))
) ENGINE=InnoDB;

-- S58: business outcome, authoritative units and supplier finality are separate.
-- The original business receipt is inserted by the same owning transaction.
CREATE TABLE IF NOT EXISTS harness_service_metering (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL, quote_id VARBINARY(64) NOT NULL,
 business_receipt VARBINARY(128) NOT NULL, original_hash BINARY(32) NOT NULL,
 state VARCHAR(24) NOT NULL, settlement_receipt VARBINARY(128) NULL, disposition_proof JSON NULL,
 PRIMARY KEY(tenant_id,product_id,service_id), UNIQUE KEY original_meter_task(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,service_id) REFERENCES service_events(tenant_id,product_id,record_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES harness_envelopes(tenant_id,product_id,task_id),
 CHECK(state IN ('PENDING_VERIFICATION','SETTLED','RELEASED'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS harness_attempt_metering (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL, service_id VARBINARY(64) NOT NULL,
 original_metadata JSON NOT NULL, original_hash BINARY(32) NOT NULL,
 usage_state VARCHAR(16) NOT NULL, input_units BIGINT UNSIGNED NULL, output_units BIGINT UNSIGNED NULL,
 evidence_receipt VARBINARY(128) NULL,
 meter_next_run_at DATETIME(6) NOT NULL, meter_failures BIGINT UNSIGNED NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,service_id) REFERENCES harness_service_metering(tenant_id,product_id,service_id),
 CHECK((usage_state='UNKNOWN' AND input_units IS NULL AND output_units IS NULL) OR
       (usage_state='AUTHORITATIVE' AND input_units IS NOT NULL AND output_units IS NOT NULL AND input_units>0 AND output_units>0))
) ENGINE=InnoDB;

-- A known program refusal is a separate fact, never a WORLD_COMMIT.
CREATE TABLE IF NOT EXISTS harness_rehearsal_negative_outcomes (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 root_task_id VARBINARY(64) NOT NULL, input_index INT UNSIGNED NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 receipt_key VARBINARY(128) NOT NULL, command_hash BINARY(32) NOT NULL, document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,root_task_id) REFERENCES harness_admin_rehearsals(tenant_id,product_id,task_id)
) ENGINE=InnoDB;

-- Fixed online adapters share the original task/attempt and its step budget.
-- No candidate body or independent prompt is stored here.
CREATE TABLE IF NOT EXISTS harness_skill_invocations (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 invocation_id VARBINARY(64) NOT NULL, binding_id VARBINARY(128) NOT NULL,
 capability VARCHAR(32) NOT NULL, definition JSON NOT NULL, definition_hash BINARY(32) NOT NULL,
 parent_invocation_id VARBINARY(64) NULL, depth INT UNSIGNED NOT NULL,
 started_at DATETIME(6) NOT NULL, deadline DATETIME(6) NOT NULL, fence BIGINT UNSIGNED NOT NULL,
 state VARCHAR(16) NOT NULL, output_hash BINARY(32) NOT NULL, receipt_key VARBINARY(128) NOT NULL, command_hash BINARY(32) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,invocation_id),
 UNIQUE(tenant_id,product_id,task_id,attempt_id,binding_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 CHECK(depth BETWEEN 1 AND 4), CHECK(deadline>started_at),
 CHECK(capability IN ('TEXT','SIGN_CUSTODY')), CHECK(state='COMPLETED')
) ENGINE=InnoDB;
