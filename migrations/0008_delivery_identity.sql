CREATE TABLE IF NOT EXISTS integration_credential_hashes (
 token_hash BINARY(32) NOT NULL,
 kind VARCHAR(16) NOT NULL,
 principal_id VARBINARY(64) NOT NULL,
 installation_id VARBINARY(64) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY (token_hash),
 UNIQUE KEY integration_hash_principal (kind,principal_id),
 CONSTRAINT integration_hash_kind CHECK (kind IN ('OPERATOR','SERVICE','CLIENT'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_registrars (
 tenant_id VARBINARY(64) NOT NULL,
 credential_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 action VARCHAR(64) NOT NULL,
 allowed_use VARCHAR(64) NOT NULL,
 expires_at DATETIME(6) NULL,
 enabled TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 bootstrap_receipt VARBINARY(64) NOT NULL,
 installation_id VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,credential_id,product_id),
 CONSTRAINT integration_registrar_right CHECK (action='customer.register' AND allowed_use='TENANT_CUSTOMER_REGISTRATION')
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_customers (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 service_subject_id VARBINARY(64) NOT NULL,
 consumer_id VARBINARY(64) NOT NULL,
 account_id VARBINARY(64) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 enabled TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 registry_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,customer_id),
 UNIQUE KEY integration_customer_service_subject_id (tenant_id,product_id,service_subject_id),
 UNIQUE KEY integration_customer_consumer_id (tenant_id,product_id,consumer_id),
 UNIQUE KEY integration_customer_account_id (tenant_id,product_id,account_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_principals (
 principal_id VARBINARY(64) NOT NULL,
 token_hash BINARY(32) NOT NULL,
 kind VARCHAR(16) NOT NULL,
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 subject_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NULL,
 issuer_service_key VARBINARY(64) NULL,
 profile VARCHAR(32) NOT NULL,
 client_actions JSON NOT NULL,
 entitlement_id VARBINARY(64) NULL,
 issued_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 revoked TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 nonce BINARY(32) NULL,
 key_version BIGINT UNSIGNED NULL,
 key_ref VARBINARY(64) NULL,
 issuance_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY (principal_id),
 UNIQUE KEY integration_principal_hash (token_hash),
 CONSTRAINT integration_principal_branch CHECK ((kind='SERVICE' AND world_instance_id IS NULL AND issuer_service_key IS NULL AND entitlement_id IS NULL AND nonce IS NULL AND key_version IS NULL AND key_ref IS NULL) OR (kind='CLIENT' AND world_instance_id IS NOT NULL AND issuer_service_key IS NOT NULL AND entitlement_id IS NOT NULL AND nonce IS NOT NULL AND key_version IS NOT NULL AND key_ref IS NOT NULL))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_external_subjects (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 external_id VARBINARY(512) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 enabled TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 registry_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,customer_id,external_id),
 UNIQUE KEY integration_mapping_player (tenant_id,product_id,customer_id,player_subject_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_entitlements (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 entitlement_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 release_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NULL,
 allowed_actions JSON NOT NULL,
 self_consent_eligible TINYINT NOT NULL,
 eligibility_policy VARCHAR(64) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 enabled TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 registry_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,customer_id,entitlement_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_world_participants (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 service_owner_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 release_id VARBINARY(64) NOT NULL,
 creation_receipt VARBINARY(64) NOT NULL,
 entitlement_id VARBINARY(64) NOT NULL,
 external_id VARBINARY(512) NOT NULL,
 enabled TINYINT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant_id,product_id,world_instance_id),
 UNIQUE KEY integration_world_player (tenant_id,product_id,world_instance_id,player_subject_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_quotes (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 quote_id VARBINARY(64) NOT NULL,
 issuer_service_key VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 project_id VARBINARY(64) NOT NULL,
 release_id VARBINARY(64) NOT NULL,
 route VARBINARY(64) NOT NULL,
 request_hash BINARY(32) NOT NULL,
 bounds JSON NOT NULL,
 customer_account_id VARBINARY(64) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 quote_receipt VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,quote_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_task_principals (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 task_id VARBINARY(64) NOT NULL,
 task_kind VARCHAR(16) NOT NULL,
 principal_kind VARCHAR(16) NOT NULL,
 principal_ref VARBINARY(64) NOT NULL,
 issuer_service_key VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 action VARCHAR(64) NOT NULL,
 accepted_at DATETIME(6) NOT NULL,
 issued_session_expires_at DATETIME(6) NOT NULL,
 original_execute_by DATETIME(6) NOT NULL,
 original_settle_by DATETIME(6) NOT NULL,
 intake_receipt VARBINARY(64) NOT NULL,
 intake_hash BINARY(32) NOT NULL,
 registration_revisions JSON NOT NULL,
 accepted_use_generation BIGINT UNSIGNED NULL,
 accepted_use_expires_at DATETIME(6) NULL,
 accepted_consent_revision BIGINT UNSIGNED NULL,
 PRIMARY KEY (tenant_id,product_id,task_id),
 CONSTRAINT integration_task_use_branch CHECK ((task_kind='TURN' AND accepted_use_generation IS NOT NULL AND accepted_use_expires_at IS NOT NULL AND accepted_consent_revision IS NOT NULL) OR (task_kind='CONTROL' AND accepted_use_generation IS NULL AND accepted_use_expires_at IS NULL AND accepted_consent_revision IS NULL))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_self_consent_current (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 policy VARCHAR(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 action VARCHAR(16) NOT NULL,
 accepted_at DATETIME(6) NULL,
 expires_at DATETIME(6) NULL,
 use_authority_generation BIGINT UNSIGNED NOT NULL,
 eligibility_ref VARBINARY(64) NOT NULL,
 receipt_ref VARBINARY(64) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,player_subject_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_self_consent_facts (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 player_subject_id VARBINARY(64) NOT NULL,
 policy VARCHAR(64) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 action VARCHAR(16) NOT NULL,
 accepted_at DATETIME(6) NULL,
 expires_at DATETIME(6) NULL,
 use_authority_generation BIGINT UNSIGNED NOT NULL,
 eligibility_ref VARBINARY(64) NOT NULL,
 receipt_ref VARBINARY(64) NOT NULL,
 principal_ref VARBINARY(64) NOT NULL,
 original_key VARBINARY(128) NOT NULL,
 payload_hash BINARY(32) NOT NULL,
 recorded_at DATETIME(6) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,player_subject_id,revision),
 UNIQUE KEY integration_consent_fact_receipt (receipt_ref)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS delivery_streams (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 update_seq BIGINT UNSIGNED NOT NULL,
 initialization_receipt VARBINARY(64) NOT NULL,
 projection_version BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant_id,product_id,world_instance_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS delivery_updates (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 update_seq BIGINT UNSIGNED NOT NULL,
 business_update_id VARBINARY(64) NOT NULL,
 kind VARCHAR(16) NOT NULL,
 result_ref VARBINARY(64) NOT NULL,
 task_ref VARBINARY(64) NOT NULL,
 event_refs JSON NOT NULL,
 packet_ref VARBINARY(64) NULL,
 delivery_state VARCHAR(16) NOT NULL,
 resource_revision BIGINT UNSIGNED NOT NULL,
 audience_refs JSON NOT NULL,
 lineage_refs JSON NOT NULL,
 PRIMARY KEY (tenant_id,product_id,world_instance_id,update_seq),
 UNIQUE KEY delivery_business_update (tenant_id,product_id,world_instance_id,business_update_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS delivery_media (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 asset_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 packet_id VARBINARY(64) NOT NULL,
 media_id VARBINARY(64) NOT NULL,
 runtime_file_ref VARBINARY(64) NULL,
 content_hash BINARY(32) NULL,
 byte_length BIGINT UNSIGNED NULL,
 mime_type VARCHAR(64) NULL,
 state VARCHAR(16) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 original_result_ref VARBINARY(64) NOT NULL,
 source_refs JSON NOT NULL,
 lease_owner VARBINARY(64) NULL,
 fence BIGINT UNSIGNED NOT NULL,
 lease_expires_at DATETIME(6) NULL,
 attempt_count BIGINT UNSIGNED NOT NULL,
 accepted_at DATETIME(6) NOT NULL,
 execute_by DATETIME(6) NOT NULL,
 maintenance_generation BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant_id,product_id,asset_id),
 UNIQUE KEY delivery_packet_media (tenant_id,product_id,world_instance_id,packet_id,media_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_notifications (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 notice_ref VARBINARY(64) NOT NULL,
 consumer_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL,
 result_ref VARBINARY(64) NOT NULL,
 initial_revision BIGINT UNSIGNED NOT NULL,
 kind VARCHAR(32) NOT NULL,
 payload_hash BINARY(32) NOT NULL,
 message_ref VARBINARY(64) NOT NULL,
 outbox_ref VARBINARY(64) NOT NULL,
 visibility_source_refs JSON NOT NULL,
 PRIMARY KEY (tenant_id,product_id,customer_id,notice_ref),
 UNIQUE KEY integration_notice_identity (tenant_id,product_id,consumer_id,world_instance_id,result_ref,initial_revision)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_challenges (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 customer_id VARBINARY(64) NOT NULL,
 consumer_id VARBINARY(64) NOT NULL,
 notice_ref VARBINARY(64) NOT NULL,
 initial_revision BIGINT UNSIGNED NOT NULL,
 nonce_hash BINARY(32) NOT NULL,
 nonce BINARY(32) NOT NULL,
 key_version BIGINT UNSIGNED NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant_id,product_id,customer_id,consumer_id,notice_ref,initial_revision)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS integration_receipts (
 tenant_id VARBINARY(64) NOT NULL,
 product_id VARBINARY(64) NOT NULL,
 owner_id VARBINARY(64) NOT NULL,
 operation VARCHAR(64) NOT NULL,
 scope_kind VARCHAR(16) NOT NULL,
 scope_address VARBINARY(256) NOT NULL,
 original_key VARBINARY(128) NOT NULL,
 receipt_ref VARBINARY(64) NOT NULL,
 payload_hash BINARY(32) NOT NULL,
 result_metadata JSON NOT NULL,
 state VARCHAR(16) NOT NULL,
 target_revision BIGINT UNSIGNED NOT NULL,
 committed_at DATETIME(6) NOT NULL,
 PRIMARY KEY (tenant_id,product_id,owner_id,operation,scope_kind,scope_address,original_key),
 UNIQUE KEY integration_receipt_ref (receipt_ref),
 CONSTRAINT integration_receipt_committed CHECK (state='COMMITTED')
) ENGINE=InnoDB;
