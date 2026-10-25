CREATE TABLE reservations (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    public_id  CHAR(36)    NOT NULL,
    slot_id    BIGINT      NOT NULL,
    user_id    BIGINT      NOT NULL,
    status     VARCHAR(20) NOT NULL,
    created_at DATETIME    NOT NULL,
    updated_at DATETIME    NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_reservations_public_id UNIQUE (public_id),
    CONSTRAINT uq_reservations_slot_user UNIQUE (slot_id, user_id),
    CONSTRAINT fk_reservations_slot FOREIGN KEY (slot_id) REFERENCES slots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_reservations_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT ck_reservations_status CHECK (status IN ('active', 'cancelled_by_user', 'cancelled_by_admin')),
    INDEX idx_reservations_slot_status (slot_id, status),
    INDEX idx_reservations_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
