-- IDs and business keys compare as bytes. No secrets or unconstrained JSON.
CREATE TABLE IF NOT EXISTS schema_migrations (
 version INT NOT NULL PRIMARY KEY, sha256 BINARY(32) NOT NULL,
 applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS tenant_gates (
 tenant_id VARBINARY(64) NOT NULL PRIMARY KEY,
 permission_version BIGINT UNSIGNED NOT NULL, enabled BOOLEAN NOT NULL,
 CHECK(OCTET_LENGTH(tenant_id)>0), CHECK(permission_version>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS product_gates (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 permission_version BIGINT UNSIGNED NOT NULL, enabled BOOLEAN NOT NULL,
 PRIMARY KEY(tenant_id,product_id), FOREIGN KEY(tenant_id) REFERENCES tenant_gates(tenant_id),
 CHECK(OCTET_LENGTH(product_id)>0), CHECK(permission_version>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS subject_governance (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, subject_id VARBINARY(64) NOT NULL,
 permission_version BIGINT UNSIGNED NOT NULL, privacy_epoch BIGINT UNSIGNED NOT NULL, view_epoch BIGINT UNSIGNED NOT NULL,
 enabled BOOLEAN NOT NULL,
 can_execute BOOLEAN NOT NULL DEFAULT 1, can_control BOOLEAN NOT NULL DEFAULT 0,
 can_govern BOOLEAN NOT NULL DEFAULT 0, can_migrate BOOLEAN NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,subject_id), FOREIGN KEY(tenant_id,product_id) REFERENCES product_gates(tenant_id,product_id),
 CHECK(OCTET_LENGTH(subject_id)>0), CHECK(permission_version>0 AND privacy_epoch>0 AND view_epoch>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS releases (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, release_id VARBINARY(64) NOT NULL,
 available BOOLEAN NOT NULL,
 PRIMARY KEY(tenant_id,product_id,release_id), FOREIGN KEY(tenant_id,product_id) REFERENCES product_gates(tenant_id,product_id),
 CHECK(OCTET_LENGTH(release_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS world_controls (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NOT NULL,
 release_id VARBINARY(64) NOT NULL, world_revision BIGINT UNSIGNED NOT NULL, control_version BIGINT UNSIGNED NOT NULL,
 privacy_epoch BIGINT UNSIGNED NOT NULL, view_epoch BIGINT UNSIGNED NOT NULL, status VARCHAR(16) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id),
 FOREIGN KEY(tenant_id,product_id,release_id) REFERENCES releases(tenant_id,product_id,release_id),
 CHECK(OCTET_LENGTH(world_instance_id)>0), CHECK(status IN ('CREATED','RUNNING','PAUSED','ENDED')),
 CHECK(control_version>0 AND privacy_epoch>0 AND view_epoch>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS commands (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, subject_id VARBINARY(64) NOT NULL,
 operation VARBINARY(64) NOT NULL, scope_kind VARCHAR(7) NOT NULL, world_instance_id VARBINARY(64) NULL,
 instance_scope VARBINARY(64) GENERATED ALWAYS AS (IFNULL(world_instance_id,'')) STORED,
 idem_key VARBINARY(128) NOT NULL, payload_hash BINARY(32) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,subject_id,operation,scope_kind,instance_scope,idem_key),
 FOREIGN KEY(tenant_id,product_id,subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 CHECK(OCTET_LENGTH(operation)>0 AND OCTET_LENGTH(idem_key)>0),
 CHECK((scope_kind='PRODUCT' AND world_instance_id IS NULL) OR (scope_kind='WORLD' AND world_instance_id IS NOT NULL AND OCTET_LENGTH(world_instance_id)>0))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS tasks (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, scope_kind VARCHAR(7) NOT NULL, world_instance_id VARBINARY(64) NULL,
 state VARCHAR(16) NOT NULL, fence BIGINT UNSIGNED NOT NULL DEFAULT 0,
 task_kind VARCHAR(16) NOT NULL DEFAULT 'TURN', CHECK(task_kind IN ('TURN','ADMIN','CONTROL','GOVERNANCE','MIGRATION')),
 lease_owner VARBINARY(64) NULL, lease_expires_at DATETIME(6) NULL, next_run_at DATETIME(6) NOT NULL,
 cancelled BOOLEAN NOT NULL DEFAULT 0, calls_used TINYINT UNSIGNED NOT NULL DEFAULT 0,
 project_id VARBINARY(64) NULL, CHECK(project_id IS NULL OR OCTET_LENGTH(project_id)>0),
 PRIMARY KEY(tenant_id,product_id,task_id), UNIQUE KEY task_owner(tenant_id,product_id,task_id,owner_subject_id),
 KEY claim_tasks(state,next_run_at,lease_expires_at,tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 CHECK(OCTET_LENGTH(task_id)>0), CHECK(state IN ('ACCEPTED','RUNNING','WAITING_EXTERNAL','UNKNOWN','COMMITTED','FAILED','CANCELLED','REVOKED')),
 CHECK(calls_used<=2), CHECK((scope_kind='PRODUCT' AND world_instance_id IS NULL) OR (scope_kind='WORLD' AND world_instance_id IS NOT NULL AND OCTET_LENGTH(world_instance_id)>0))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS attempts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, attempt_id VARBINARY(64) NOT NULL,
 slot TINYINT UNSIGNED NOT NULL, request_key VARBINARY(128) NOT NULL, route VARBINARY(64) NOT NULL, request_hash BINARY(32) NOT NULL,
 state VARCHAR(16) NOT NULL, send_started_at DATETIME(6) NULL, marker_commit_key VARBINARY(128) NULL, provider_request_id VARBINARY(128) NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id), UNIQUE KEY attempt_slot(tenant_id,product_id,task_id,slot),
 UNIQUE KEY attempt_request(tenant_id,product_id,request_key),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 CHECK(OCTET_LENGTH(attempt_id)>0 AND OCTET_LENGTH(request_key)>0 AND OCTET_LENGTH(route)>0), CHECK(slot<2),
 CHECK(state IN ('RESERVED','AUTHORIZED','SENT','PENDING','UNKNOWN','SUCCEEDED','FAILED','CANCEL_CONFIRMED')),
 CHECK((send_started_at IS NULL AND state IN ('RESERVED','AUTHORIZED','FAILED','CANCEL_CONFIRMED') AND marker_commit_key IS NULL) OR (send_started_at IS NOT NULL AND marker_commit_key IS NOT NULL))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS dispatch_authorizations (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 attempt_id VARBINARY(64) NOT NULL, authorization_id VARBINARY(64) NOT NULL, fence BIGINT UNSIGNED NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id,attempt_id), UNIQUE KEY authorization_key(tenant_id,product_id,authorization_id),
 FOREIGN KEY(tenant_id,product_id,task_id,attempt_id) REFERENCES attempts(tenant_id,product_id,task_id,attempt_id),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id),
 CHECK(OCTET_LENGTH(authorization_id)>0 AND fence>0)
) ENGINE=InnoDB;
-- GLOBAL is an addressing discriminator, never an invented tenant/product identity.
CREATE TABLE IF NOT EXISTS budget_accounts (
 kind VARCHAR(24) NOT NULL, scope_key VARBINARY(256) NOT NULL, account_id VARBINARY(64) NOT NULL,
 tenant_id VARBINARY(64) NULL, product_id VARBINARY(64) NULL,
 balance BIGINT NOT NULL, held BIGINT NOT NULL DEFAULT 0, revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 world_instance_id VARBINARY(64) NULL, project_id VARBINARY(64) NULL,
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 PRIMARY KEY(kind,scope_key,account_id), FOREIGN KEY(tenant_id,product_id) REFERENCES product_gates(tenant_id,product_id),
 CHECK(kind IN ('supplier_global','supplier_tenant','platform_risk','customer','project','world')),
 CHECK(OCTET_LENGTH(scope_key)>0 AND OCTET_LENGTH(account_id)>0), CHECK(balance>=0 AND held>=0 AND held<=balance),
 CHECK((kind='world' AND world_instance_id IS NOT NULL AND OCTET_LENGTH(world_instance_id)>0) OR (kind<>'world' AND world_instance_id IS NULL)),
 CHECK((kind='project' AND project_id IS NOT NULL AND OCTET_LENGTH(project_id)>0) OR (kind<>'project' AND project_id IS NULL)),
 CHECK((kind IN ('supplier_global','platform_risk') AND scope_key='GLOBAL' AND tenant_id IS NULL AND product_id IS NULL) OR
 (kind='supplier_tenant' AND tenant_id IS NOT NULL AND product_id IS NULL) OR
 (kind IN ('customer','project','world') AND tenant_id IS NOT NULL AND product_id IS NOT NULL))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS reservations (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 kind VARCHAR(24) NOT NULL, scope_key VARBINARY(256) NOT NULL, account_id VARBINARY(64) NOT NULL, reservation_id VARBINARY(64) NOT NULL,
 purpose VARBINARY(64) NOT NULL, amount BIGINT NOT NULL, status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 PRIMARY KEY(kind,scope_key,account_id,reservation_id), UNIQUE KEY task_reservation(tenant_id,product_id,task_id,kind,scope_key,account_id,purpose),
 FOREIGN KEY(kind,scope_key,account_id) REFERENCES budget_accounts(kind,scope_key,account_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 CHECK(amount>=0), CHECK(OCTET_LENGTH(reservation_id)>0 AND OCTET_LENGTH(purpose)>0), CHECK(status IN ('HELD','SETTLED','RELEASED','RISK'))
) ENGINE=InnoDB;
-- New business rows are protected by byte-addressed mutexes acquired at their plan rank.
CREATE TABLE IF NOT EXISTS lock_markers (
 lock_key VARBINARY(1024) NOT NULL PRIMARY KEY, CHECK(OCTET_LENGTH(lock_key)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS inbox (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, consumer_id VARBINARY(64) NOT NULL, message_id VARBINARY(128) NOT NULL,
 task_id VARBINARY(64) NOT NULL, status VARCHAR(16) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,consumer_id,message_id), FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 CHECK(OCTET_LENGTH(consumer_id)>0 AND OCTET_LENGTH(message_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS events (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NULL,
 status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL, sequence_no BIGINT UNSIGNED NULL,
 PRIMARY KEY(tenant_id,product_id,record_id), UNIQUE KEY world_sequence(tenant_id,product_id,world_instance_id,sequence_no),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id), CHECK(OCTET_LENGTH(record_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS approved_results (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL, status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(tenant_id,product_id,record_id), UNIQUE KEY task_result(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id), CHECK(OCTET_LENGTH(record_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS service_events (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL, status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(tenant_id,product_id,record_id), FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id), CHECK(OCTET_LENGTH(record_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS postings (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 kind VARCHAR(24) NOT NULL, scope_key VARBINARY(256) NOT NULL, account_id VARBINARY(64) NOT NULL,
 amount BIGINT NOT NULL, status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(tenant_id,product_id,record_id), FOREIGN KEY(kind,scope_key,account_id) REFERENCES budget_accounts(kind,scope_key,account_id),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id), CHECK(OCTET_LENGTH(record_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS outbox (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL, consumer_id VARBINARY(64) NOT NULL, message_id VARBINARY(128) NOT NULL,
 status VARCHAR(16) NOT NULL, revision BIGINT UNSIGNED NOT NULL, lease_owner VARBINARY(64) NULL, lease_expires_at DATETIME(6) NULL,
 PRIMARY KEY(tenant_id,product_id,record_id), UNIQUE KEY outbox_message(tenant_id,product_id,consumer_id,message_id), KEY delivery_claim(status,lease_expires_at),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id),
 CHECK(OCTET_LENGTH(record_id)>0 AND OCTET_LENGTH(consumer_id)>0 AND OCTET_LENGTH(message_id)>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS commit_receipts (
 commit_key VARBINARY(128) NOT NULL PRIMARY KEY,
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 scope_kind VARCHAR(7) NOT NULL, world_instance_id VARBINARY(64) NULL, task_id VARBINARY(64) NULL,
 instance_scope VARBINARY(64) GENERATED ALWAYS AS (IFNULL(world_instance_id,'')) STORED,
 operation VARBINARY(64) NOT NULL, command_key VARBINARY(128) NOT NULL, payload_hash BINARY(32) NOT NULL,
 phase VARCHAR(16) NOT NULL, CHECK(phase IN ('COMMIT','AUTHORIZE','SEND')),
 UNIQUE KEY command_phase_commit(tenant_id,product_id,owner_subject_id,operation,scope_kind,instance_scope,command_key,phase),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 committed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 FOREIGN KEY(tenant_id,product_id,task_id,owner_subject_id) REFERENCES tasks(tenant_id,product_id,task_id,owner_subject_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 CHECK(OCTET_LENGTH(commit_key)>0 AND OCTET_LENGTH(command_key)>0 AND OCTET_LENGTH(operation)>0),
 CHECK((scope_kind='PRODUCT' AND world_instance_id IS NULL) OR (scope_kind='WORLD' AND world_instance_id IS NOT NULL AND OCTET_LENGTH(world_instance_id)>0))
) ENGINE=InnoDB;
