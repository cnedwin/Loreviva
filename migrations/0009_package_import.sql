CREATE TABLE IF NOT EXISTS content_package_imports (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 import_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 package_id VARBINARY(96) NOT NULL, package_version VARBINARY(100) NOT NULL,
 copy_key VARBINARY(128) NOT NULL DEFAULT '', content_hash BINARY(32) NOT NULL,
 mapping_hash BINARY(32) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,import_id),
 UNIQUE KEY import_slot(tenant_id,product_id,owner_subject_id,package_id,package_version,copy_key),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;

-- Protected origin metadata is independent of editable definitions and revisions.
-- Links are sticky. Deleting an object or archiving a draft does not promote it.
CREATE TABLE IF NOT EXISTS content_package_import_lineage (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 resource_kind VARCHAR(8) NOT NULL, resource_id VARBINARY(64) NOT NULL,
 import_id VARBINARY(64) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,resource_kind,resource_id,import_id),
 FOREIGN KEY(tenant_id,product_id,import_id) REFERENCES content_package_imports(tenant_id,product_id,import_id),
 CHECK(resource_kind IN ('DRAFT','OBJECT'))
) ENGINE=InnoDB;
