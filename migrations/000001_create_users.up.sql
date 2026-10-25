CREATE TABLE users (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    keycloak_sub VARCHAR(255) NOT NULL,
    created_at   DATETIME     NOT NULL,
    updated_at   DATETIME     NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_users_keycloak_sub UNIQUE (keycloak_sub)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
