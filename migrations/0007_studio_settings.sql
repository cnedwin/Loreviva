CREATE TABLE IF NOT EXISTS studio_credential_state (
 credential_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 expires_at DATETIME(6) NULL,
 PRIMARY KEY (credential_id),
 CONSTRAINT studio_credential_revision CHECK (revision > 0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_operator_grants (
 tenant_id VARBINARY(64) NOT NULL,
 credential_id VARBINARY(64) NOT NULL,
 scope_type VARCHAR(7) NOT NULL,
 product_id VARBINARY(64) NULL,
 scope_address VARBINARY(66) NOT NULL,
 action VARBINARY(64) NOT NULL,
 allowed_use VARBINARY(32) NOT NULL,
 effect VARCHAR(5) NOT NULL,
 enabled TINYINT NOT NULL,
 expires_at DATETIME(6) NULL,
 revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant_id,credential_id,scope_type,scope_address,action,allowed_use),
 CONSTRAINT studio_grants_scope_type CHECK (scope_type in ('TENANT','PRODUCT')),
 CONSTRAINT studio_grants_scope_branch CHECK ((scope_type = 'TENANT') = (product_id is null)),
 CONSTRAINT studio_grants_scope_address CHECK (scope_address = if((scope_type = 'TENANT'),'T',concat('P:',product_id))),
 CONSTRAINT studio_grants_effect CHECK (effect in ('ALLOW','DENY')),
 CONSTRAINT studio_grants_enabled CHECK (enabled in (0,1)),
 CONSTRAINT studio_grants_revision CHECK (revision > 0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_self_consent_current (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 subject_id VARBINARY(64) NOT NULL,
 credential_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 last_action VARCHAR(16) NOT NULL,
 accepted_at DATETIME(6) NULL,
 expires_at DATETIME(6) NULL,
 legacy_baseline TINYINT NOT NULL,
 PRIMARY KEY (tenant_id,product_id,subject_id),
 CONSTRAINT studio_consent_revision CHECK (revision > 0),
 CONSTRAINT studio_consent_action CHECK (last_action in ('LEGACY_FALSE','LEGACY_TRUE','INITIAL','RENEW','REVOKE'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_self_consent_facts (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 subject_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 credential_id VARBINARY(64) NOT NULL,
 command_key VARBINARY(128) NULL,
 action VARCHAR(16) NOT NULL,
 accepted_at DATETIME(6) NULL,
 expires_at DATETIME(6) NULL,
 recorded_at DATETIME(6) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,subject_id,revision)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_reference_catalog (
 tenant_id VARBINARY(64) NOT NULL,
 scope_type VARCHAR(7) NOT NULL,
 product_id VARBINARY(64) NULL,
 scope_address VARBINARY(66) NOT NULL,
 ref_kind VARBINARY(32) NOT NULL,
 ref_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 enabled TINYINT NOT NULL,
 document JSON NOT NULL,
 PRIMARY KEY (tenant_id,scope_type,scope_address,ref_kind,ref_id),
 CONSTRAINT studio_catalog_scope_type CHECK (scope_type in ('TENANT','PRODUCT')),
 CONSTRAINT studio_catalog_scope_branch CHECK ((scope_type = 'TENANT') = (product_id is null)),
 CONSTRAINT studio_catalog_scope_address CHECK (scope_address = if((scope_type = 'TENANT'),'T',concat('P:',product_id))),
 CONSTRAINT studio_catalog_revision CHECK (revision > 0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_connection_metadata (
 tenant_id VARBINARY(64) NOT NULL,
 scope_type VARCHAR(7) NOT NULL,
 product_id VARBINARY(64) NULL,
 scope_address VARBINARY(66) NOT NULL,
 connection_id VARBINARY(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 document JSON NOT NULL,
 PRIMARY KEY (tenant_id,scope_type,scope_address,connection_id),
 CONSTRAINT studio_connection_scope_type CHECK (scope_type in ('TENANT','PRODUCT')),
 CONSTRAINT studio_connection_scope_branch CHECK ((scope_type = 'TENANT') = (product_id is null)),
 CONSTRAINT studio_connection_scope_address CHECK (scope_address = if((scope_type = 'TENANT'),'T',concat('P:',product_id))),
 CONSTRAINT studio_connection_revision CHECK (revision > 0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_scoped_settings (
 tenant_id VARBINARY(64) NOT NULL,
 scope_type VARCHAR(7) NOT NULL,
 product_id VARBINARY(64) NULL,
 scope_address VARBINARY(66) NOT NULL,
 section VARBINARY(128) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 document JSON NOT NULL,
 PRIMARY KEY (tenant_id,scope_type,scope_address,section),
 CONSTRAINT studio_settings_scope_type CHECK (scope_type in ('TENANT','PRODUCT')),
 CONSTRAINT studio_settings_scope_branch CHECK ((scope_type = 'TENANT') = (product_id is null)),
 CONSTRAINT studio_settings_scope_address CHECK (scope_address = if((scope_type = 'TENANT'),'T',concat('P:',product_id))),
 CONSTRAINT studio_settings_revision CHECK (revision > 0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS studio_settings_commands (
 tenant_id VARBINARY(64) NOT NULL,
 scope_type VARCHAR(7) NOT NULL,
 product_id VARBINARY(64) NULL,
 scope_address VARBINARY(66) NOT NULL,
 credential_id VARBINARY(64) NOT NULL,
 family VARBINARY(16) NOT NULL,
 command_key VARBINARY(128) NOT NULL,
 kind VARBINARY(32) NOT NULL,
 payload_hash BINARY(32) NOT NULL,
 receipt JSON NOT NULL,
 committed_at DATETIME(6) NOT NULL,
 PRIMARY KEY (tenant_id,scope_type,scope_address,credential_id,family,command_key),
 CONSTRAINT studio_commands_scope_type CHECK (scope_type in ('TENANT','PRODUCT')),
 CONSTRAINT studio_commands_scope_branch CHECK ((scope_type = 'TENANT') = (product_id is null)),
 CONSTRAINT studio_commands_scope_address CHECK (scope_address = if((scope_type = 'TENANT'),'T',concat('P:',product_id)))
) ENGINE=InnoDB;
ALTER TABLE operator_credentials ADD UNIQUE INDEX studio_unique_bearer (token_sha256);
ALTER TABLE registered_jobs ADD COLUMN issuer_credential_id VARBINARY(64) NULL;
ALTER TABLE registered_jobs ADD COLUMN issuer_provenance VARCHAR(24) NULL;
