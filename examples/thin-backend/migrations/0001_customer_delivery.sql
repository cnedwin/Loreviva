-- Customer-owned delivery tables for the synthetic reference integration.
-- Apply only to the separate customer database through its initialization flow.
-- CREATE intentionally fails on existing tables; this is not an upgrade migration.
-- Fixed summary values carry no original conversation or model-generated body.
-- Owning code validates ASCII/Identity/scope, raw32 hashes and actual original notice revision.
-- Both rows commit in one customer transaction. No platform/cross-DB authority is created.
-- created_at must be actual same-transaction customer DBUTC, never client time or fabricated history.

CREATE TABLE customer_inbox (
    installation_id VARBINARY(64) NOT NULL,
    customer_id VARBINARY(64) NOT NULL,
    consumer_id VARBINARY(64) NOT NULL,
    notification_ref VARBINARY(64) NOT NULL,
    initial_revision BIGINT UNSIGNED NOT NULL,
    payload_hash BINARY(32) NOT NULL,
    message_id VARBINARY(64) NOT NULL,
    status VARCHAR(9) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (installation_id, customer_id, consumer_id, notification_ref, initial_revision),
    CONSTRAINT customer_inbox_committed CHECK (status = 'COMMITTED'),
    CONSTRAINT customer_inbox_ids CHECK (OCTET_LENGTH(installation_id) > 0 AND OCTET_LENGTH(customer_id) > 0 AND OCTET_LENGTH(consumer_id) > 0 AND OCTET_LENGTH(notification_ref) > 0 AND OCTET_LENGTH(message_id) > 0)
) ENGINE=InnoDB;

CREATE TABLE customer_messages (
    installation_id VARBINARY(64) NOT NULL,
    customer_id VARBINARY(64) NOT NULL,
    consumer_id VARBINARY(64) NOT NULL,
    notification_ref VARBINARY(64) NOT NULL,
    initial_revision BIGINT UNSIGNED NOT NULL,
    message_id VARBINARY(64) NOT NULL,
    safe_summary VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    message_hash BINARY(32) NOT NULL,
    PRIMARY KEY (installation_id, customer_id, consumer_id, notification_ref, initial_revision),
    CONSTRAINT customer_messages_summary CHECK (safe_summary IN ('SYNTHETIC_RESULT_COMMITTED', 'SYNTHETIC_CONTROL_COMMITTED')),
    CONSTRAINT customer_messages_ids CHECK (OCTET_LENGTH(installation_id) > 0 AND OCTET_LENGTH(customer_id) > 0 AND OCTET_LENGTH(consumer_id) > 0 AND OCTET_LENGTH(notification_ref) > 0 AND OCTET_LENGTH(message_id) > 0)
) ENGINE=InnoDB;

