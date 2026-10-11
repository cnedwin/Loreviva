ALTER TABLE world_controls DROP CHECK world_controls_chk_3, ADD CONSTRAINT runtime_initial_control CHECK ((control_version>0 OR (control_version=0 AND status='CREATED')) AND privacy_epoch>0 AND view_epoch>0);
CREATE TABLE IF NOT EXISTS runtime_events (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, record_id VARBINARY(64) NOT NULL,
 document MEDIUMBLOB NOT NULL, recorded_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(tenant_id,product_id,record_id),
 FOREIGN KEY(tenant_id,product_id,record_id) REFERENCES events(tenant_id,product_id,record_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS runtime_instances (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, project_id VARBINARY(64) NOT NULL, definition_id VARBINARY(64) NOT NULL,
 mode VARCHAR(16) NOT NULL, initial_release_id VARBINARY(64) NOT NULL, initialization MEDIUMBLOB NOT NULL,
 active_migration_task VARBINARY(64) NULL, last_migration_task VARBINARY(64) NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 FOREIGN KEY(tenant_id,product_id,owner_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 CHECK(mode='PRIVATE')
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS actors (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NOT NULL,
 actor_id VARBINARY(64) NOT NULL, slot_id VARBINARY(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id,actor_id), UNIQUE KEY actor_slot(tenant_id,product_id,world_instance_id,slot_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES runtime_instances(tenant_id,product_id,world_instance_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS items (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, world_instance_id VARBINARY(64) NOT NULL,
 item_id VARBINARY(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id,item_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES runtime_instances(tenant_id,product_id,world_instance_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS runtime_tasks (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS migration_plans (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, plan_id VARBINARY(64) NOT NULL, owner_subject_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, state_hash CHAR(64) NOT NULL, plan_hash CHAR(64) NOT NULL, document MEDIUMBLOB NOT NULL,
 PRIMARY KEY(tenant_id,product_id,plan_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES runtime_instances(tenant_id,product_id,world_instance_id)
) ENGINE=InnoDB;
