CREATE TABLE IF NOT EXISTS file_staging (
 sequence_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 storage_key VARBINARY(32) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 state VARCHAR(16) NOT NULL DEFAULT 'ACTIVE',
 PRIMARY KEY(sequence_id), UNIQUE KEY staging_key(tenant_id,product_id,storage_key),
 KEY staging_page(tenant_id,product_id,sequence_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 CHECK(state IN ('ACTIVE','DELETE_INTENT','DELETED'))
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS file_cleanup_tasks (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 cutoff DATETIME(6) NOT NULL, upper_sequence BIGINT UNSIGNED NOT NULL,
 cursor_sequence BIGINT UNSIGNED NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES registered_jobs(tenant_id,product_id,task_id),
 CHECK(cursor_sequence<=upper_sequence)
) ENGINE=InnoDB;
