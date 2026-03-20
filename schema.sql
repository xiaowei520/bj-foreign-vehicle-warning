-- ================================================================
-- 进京证摄像头情报站 · MySQL Schema
-- 在 Railway MySQL 的 Query 界面执行
-- ================================================================

CREATE DATABASE IF NOT EXISTS camera_intel CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE camera_intel;

-- 1. 摄像头点位表
CREATE TABLE IF NOT EXISTS cameras (
  id             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  lng            DOUBLE NOT NULL,
  lat            DOUBLE NOT NULL,
  address        VARCHAR(255) DEFAULT '',
  status         ENUM('pending','active','inactive') NOT NULL DEFAULT 'pending',
  report_count   INT UNSIGNED NOT NULL DEFAULT 1,
  last_report_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  confidence     DECIMAL(5,1) NOT NULL DEFAULT 100.0,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_status (status),
  INDEX idx_location (lng, lat)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 用户上报表
CREATE TABLE IF NOT EXISTS reports (
  id             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  camera_id      BIGINT UNSIGNED NOT NULL,
  screenshot_url TEXT NOT NULL,
  description    TEXT,
  plate_province VARCHAR(10) DEFAULT '',
  status         ENUM('pending','approved','rejected') NOT NULL DEFAULT 'pending',
  reviewer_note  TEXT,
  reported_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  reviewed_at    DATETIME,
  INDEX idx_camera (camera_id),
  INDEX idx_status (status),
  FOREIGN KEY (camera_id) REFERENCES cameras(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 3. 社区评论表
CREATE TABLE IF NOT EXISTS comments (
  id           BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  camera_id    BIGINT UNSIGNED NOT NULL,
  nickname     VARCHAR(50) NOT NULL DEFAULT '匿名',
  content      TEXT NOT NULL,
  comment_type ENUM('confirm','deny','info') NOT NULL DEFAULT 'info',
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_camera (camera_id),
  INDEX idx_time (created_at DESC),
  FOREIGN KEY (camera_id) REFERENCES cameras(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
