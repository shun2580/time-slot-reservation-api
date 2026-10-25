CREATE TABLE reservation_histories (
    id             BIGINT      NOT NULL AUTO_INCREMENT,
    reservation_id BIGINT      NOT NULL,
    from_status    VARCHAR(20) NULL,
    to_status      VARCHAR(20) NOT NULL,
    actor_user_id  BIGINT      NOT NULL,
    occurred_at    DATETIME    NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT fk_reservation_histories_reservation FOREIGN KEY (reservation_id) REFERENCES reservations (id) ON DELETE RESTRICT,
    CONSTRAINT fk_reservation_histories_actor FOREIGN KEY (actor_user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT ck_reservation_histories_from_status CHECK (from_status IS NULL OR from_status IN ('active', 'cancelled_by_user', 'cancelled_by_admin')),
    CONSTRAINT ck_reservation_histories_to_status CHECK (to_status IN ('active', 'cancelled_by_user', 'cancelled_by_admin')),
    INDEX idx_reservation_histories_reservation (reservation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
