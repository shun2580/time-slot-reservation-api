CREATE TABLE slots (
    id                      BIGINT       NOT NULL AUTO_INCREMENT,
    public_id               CHAR(36)     NOT NULL,
    title                   VARCHAR(100) NOT NULL,
    capacity                INT UNSIGNED NOT NULL,
    start_at                DATETIME     NOT NULL,
    end_at                  DATETIME     NOT NULL,
    reservation_deadline_at DATETIME     NOT NULL,
    status                  VARCHAR(20)  NOT NULL,
    created_at              DATETIME     NOT NULL,
    updated_at              DATETIME     NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_slots_public_id UNIQUE (public_id),
    CONSTRAINT ck_slots_status CHECK (status IN ('draft', 'published', 'closed')),
    CONSTRAINT ck_slots_start_before_end CHECK (start_at < end_at),
    CONSTRAINT ck_slots_deadline_not_after_start CHECK (reservation_deadline_at <= start_at),
    CONSTRAINT ck_slots_capacity_positive CHECK (capacity > 0),
    INDEX idx_slots_status_start_at (status, start_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
