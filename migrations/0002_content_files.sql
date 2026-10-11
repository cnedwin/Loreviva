-- Immutable definitions and recoverable private-file lifecycle. No secrets.
CREATE TABLE IF NOT EXISTS world_projects (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, project_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, name VARCHAR(320) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,project_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS content_drafts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, draft_id VARBINARY(64) NOT NULL,
 project_id VARBINARY(64) NULL, owner_subject_id VARBINARY(64) NOT NULL,
 root_kind VARCHAR(24) NOT NULL, current_revision BIGINT UNSIGNED NOT NULL,
 state VARCHAR(24) NOT NULL, deleted BOOLEAN NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,draft_id),
 FOREIGN KEY(tenant_id,product_id,project_id) REFERENCES world_projects(tenant_id,product_id,project_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 CHECK(current_revision>0), CHECK(state IN ('DRAFT','ARCHIVED','ISOLATED_IMPORT')),
 CHECK(root_kind IN ('WORLD','CHARACTER','ROLE','SKILL','KNOWLEDGE','PRESENTATION_THEME'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS content_revisions (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, draft_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL, content_hash BINARY(32) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,draft_id,revision),
 FOREIGN KEY(tenant_id,product_id,draft_id) REFERENCES content_drafts(tenant_id,product_id,draft_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS content_releases (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, release_id VARBINARY(64) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, draft_revision BIGINT UNSIGNED NOT NULL, content_hash BINARY(32) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,release_id),
 FOREIGN KEY(tenant_id,product_id,release_id) REFERENCES releases(tenant_id,product_id,release_id),
 FOREIGN KEY(tenant_id,product_id,draft_id,draft_revision) REFERENCES content_revisions(tenant_id,product_id,draft_id,revision)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS content_checks (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, report_id VARBINARY(64) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, draft_revision BIGINT UNSIGNED NOT NULL, report_kind VARCHAR(24) NOT NULL,
 document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,report_id),
 FOREIGN KEY(tenant_id,product_id,draft_id,draft_revision) REFERENCES content_revisions(tenant_id,product_id,draft_id,revision)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS admin_results (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, result_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, schema_name VARBINARY(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,result_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS assets (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, asset_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, state VARCHAR(16) NOT NULL,
 storage_key VARBINARY(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,asset_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 CHECK(state IN ('PENDING','READY','FAILED','DELETED'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS seed_receipts (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, seed_version VARBINARY(64) NOT NULL,
 document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,seed_version),
 FOREIGN KEY(tenant_id,product_id) REFERENCES product_gates(tenant_id,product_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS governance_watermark (
 singleton TINYINT NOT NULL PRIMARY KEY, applied_watermark BIGINT UNSIGNED NOT NULL,
 required_watermark BIGINT UNSIGNED NOT NULL, fresh_synthetic BOOLEAN NOT NULL,
 CHECK(singleton=1), CHECK(applied_watermark>=required_watermark)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS operator_credentials (
 credential_id VARBINARY(64) NOT NULL PRIMARY KEY, tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL, subject_id VARBINARY(64) NOT NULL,
 token_sha256 BINARY(32) NOT NULL, enabled BOOLEAN NOT NULL,
 FOREIGN KEY(tenant_id,product_id,subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS operator_actions (
 credential_id VARBINARY(64) NOT NULL, action VARBINARY(64) NOT NULL,
 PRIMARY KEY(credential_id,action), FOREIGN KEY(credential_id) REFERENCES operator_credentials(credential_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS registered_jobs (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 operation VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id)
) ENGINE=InnoDB;
