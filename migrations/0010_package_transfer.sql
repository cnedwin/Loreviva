CREATE TABLE IF NOT EXISTS content_package_adoptions (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 draft_id VARBINARY(64) NOT NULL, revision BIGINT UNSIGNED NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, content_hash BINARY(32) NOT NULL,
 document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,draft_id,revision),
 FOREIGN KEY(tenant_id,product_id,draft_id) REFERENCES content_drafts(tenant_id,product_id,draft_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS content_package_exports (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 export_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 release_id VARBINARY(64) NOT NULL, archive_hash BINARY(32) NOT NULL,
 source_hash BINARY(32) NOT NULL, archive LONGBLOB NOT NULL,
 document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,export_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;
