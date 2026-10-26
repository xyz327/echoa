-- ==========================================
-- Echoa 本地联调初始化数据脚本
-- ==========================================

CREATE DATABASE IF NOT EXISTS test CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE test;

-- 1. 普通业务用户表（可供 mysql_list_tables / mysql_describe_table / mysql_query 正常访问）
CREATE TABLE IF NOT EXISTS users (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(64) NOT NULL UNIQUE COMMENT '用户账号',
    email VARCHAR(128) NOT NULL COMMENT '邮箱',
    role VARCHAR(32) NOT NULL DEFAULT 'member' COMMENT '角色权限',
    active TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用: 1启用 0禁用',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '注册时间'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户基础表';

INSERT INTO users (username, email, role, active) VALUES
('alice', 'alice@example.com', 'admin', 1),
('bob', 'bob@example.com', 'developer', 1),
('charlie', 'charlie@example.com', 'analyst', 1),
('dave', 'dave@example.com', 'member', 0),
('eva', 'eva@example.com', 'developer', 1);

-- 2. 订单表（包含多行数据测试 LIMIT 改写与 64KB 截断）
CREATE TABLE IF NOT EXISTS orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL COMMENT '关联用户ID',
    order_no VARCHAR(64) NOT NULL UNIQUE COMMENT '订单号',
    amount DECIMAL(10, 2) NOT NULL DEFAULT 0.00 COMMENT '订单金额',
    status VARCHAR(32) NOT NULL DEFAULT 'paid' COMMENT '订单状态',
    extra_notes TEXT COMMENT '备注长文本',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '下单时间',
    INDEX idx_user_id (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='业务订单表';

INSERT INTO orders (user_id, order_no, amount, status, extra_notes) VALUES
(1, 'ORD-20261026-0001', 199.50, 'completed', 'Urgent shipping requested'),
(1, 'ORD-20261026-0002', 89.00, 'paid', 'Standard delivery'),
(2, 'ORD-20261026-0003', 1299.00, 'shipped', 'Enterprise license renewal'),
(3, 'ORD-20261026-0004', 45.00, 'pending', 'Awaiting payment confirmation');

-- 3. 敏感凭证黑名单表（与 config.yaml 中的 table_blacklist 对应，用于验证安全拦截）
CREATE TABLE IF NOT EXISTS user_password (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    salt VARCHAR(64) NOT NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='敏感密码表(黑名单)';

INSERT INTO user_password (user_id, password_hash, salt) VALUES
(1, '5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8', 's@lt1'),
(2, '4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a', 's@lt2');

CREATE TABLE IF NOT EXISTS auth_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    refresh_token VARCHAR(255) NOT NULL,
    expires_at TIMESTAMP NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='鉴权Token表(黑名单)';

INSERT INTO auth_tokens (user_id, refresh_token, expires_at) VALUES
(1, 'rt_live_abcdef1234567890', '2026-11-01 00:00:00');
