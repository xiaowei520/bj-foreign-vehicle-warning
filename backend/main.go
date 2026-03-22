package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// ─── Config ───────────────────────────────────────────────────────────────────
var (
	db                *sql.DB
	imgbbKey          = getEnv("IMGBB_KEY", "")           // 腐牛图床 API Key
	feishuWebhook     = getEnv("FEISHU_WEBHOOK", "")
	reviewCallbackURL = getEnv("REVIEW_CALLBACK_URL", "http://localhost:8080")
	adminToken        = getEnv("ADMIN_TOKEN", "changeme")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Models ───────────────────────────────────────────────────────────────────
type Camera struct {
	ID           uint64    `json:"id"`
	Lng          float64   `json:"lng"`
	Lat          float64   `json:"lat"`
	Address      string    `json:"address"`
	Status       string    `json:"status"`
	ReportCount  int       `json:"report_count"`
	LastReportAt time.Time `json:"last_report_at"`
	Confidence   float64   `json:"confidence"`
	CreatedAt    time.Time `json:"created_at"`
	UpdateTime   string    `json:"update_time"`
}

type Report struct {
	ID            uint64    `json:"id"`
	CameraID      uint64    `json:"camera_id"`
	ScreenshotURL string    `json:"screenshot_url"`
	Description   string    `json:"description"`
	PlateProvince string    `json:"plate_province"`
	Status        string    `json:"status"`
	ReviewerNote  string    `json:"reviewer_note"`
	ReportedAt    time.Time `json:"reported_at"`
}

type Comment struct {
	ID          uint64    `json:"id"`
	CameraID    uint64    `json:"camera_id"`
	Nickname    string    `json:"nickname"`
	Content     string    `json:"content"`
	CommentType string    `json:"comment_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// ─── Main ─────────────────────────────────────────────────────────────────────
func main() {
	dsn := getEnv("MYSQL_URL", "")
	if dsn == "" {
		// Railway 注入格式: user:pass@tcp(host:port)/dbname
		host := getEnv("MYSQLHOST", "localhost")
		port := getEnv("MYSQLPORT", "3306")
		user := getEnv("MYSQLUSER", "root")
		pass := getEnv("MYSQLPASSWORD", "")
		name := getEnv("MYSQLDATABASE", "camera_intel")
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&loc=UTC",
			user, pass, host, port, name)
	}

	var err error
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("DB open failed: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Minute * 3)

	if err = db.Ping(); err != nil {
		log.Fatalf("DB ping failed: %v", err)
	}
	log.Println("✅ MySQL connected")

	// 自动建表
	if err = migrate(); err != nil {
		log.Fatalf("migrate failed: %v", err)
	}

	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:    []string{"Content-Type", "Authorization"},
	}))

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	// 摄像头
	r.GET("/api/cameras", getCameras)
	r.POST("/api/report", submitReport)
	r.GET("/api/cameras/:id/reports", getCameraReports)

	// 评论
	r.GET("/api/cameras/:id/comments", getComments)
	r.POST("/api/cameras/:id/comments", postComment)

	// 审核回调
	r.GET("/api/review/callback", reviewCallback)

	port := getEnv("PORT", "8080")
	log.Printf("🚀 Server on :%s", port)
	r.Run(":" + port)
}

// ─── 自动建表 ─────────────────────────────────────────────────────────────────
func migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cameras (
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
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS reports (
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
			INDEX idx_status (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS comments (
			id           BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
			camera_id    BIGINT UNSIGNED NOT NULL,
			nickname     VARCHAR(50) NOT NULL DEFAULT '匿名',
			content      TEXT NOT NULL,
			comment_type ENUM('confirm','deny','info') NOT NULL DEFAULT 'info',
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_camera (camera_id),
			INDEX idx_time (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	log.Println("✅ Tables ready")
	return nil
}

// ─── GET /api/cameras ─────────────────────────────────────────────────────────
func getCameras(c *gin.Context) {
	rows, err := db.Query(`
		SELECT id, lng, lat, address, status, report_count, last_report_at, confidence, created_at
		FROM cameras WHERE status IN ('active','inactive') ORDER BY created_at DESC
	`)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var cameras []Camera
	for rows.Next() {
		var cam Camera
		if err := rows.Scan(&cam.ID, &cam.Lng, &cam.Lat, &cam.Address,
			&cam.Status, &cam.ReportCount, &cam.LastReportAt,
			&cam.Confidence, &cam.CreatedAt); err != nil {
			continue
		}
		// 时间衰变（半衰期 180 天）
		days := time.Since(cam.LastReportAt).Hours() / 24
		cam.Confidence = math.Round(cam.Confidence*math.Pow(0.5, days/180)*10) / 10
		if cam.Confidence < 0 {
			cam.Confidence = 0
		}
		if cam.Confidence < 20 && cam.Status == "active" {
			cam.Status = "inactive"
		}
		cam.UpdateTime = cam.LastReportAt.Format("2006-01-02")
		cameras = append(cameras, cam)
	}
	if cameras == nil {
		cameras = []Camera{}
	}
	c.JSON(200, cameras)
}

// ─── Claude 内容合规校验 ──────────────────────────────────────────────────────
var claudeKey = getEnv("ANTHROPIC_API_KEY", "")

type claudeResp struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// moderateText 返回 "" 表示合规，否则返回拒绝原因
func moderateText(texts ...string) string {
	if claudeKey == "" {
		log.Println("[合规] ANTHROPIC_API_KEY 未配置，跳过校验")
		return "" // 未配置 key 则跳过校验
	}
	combined := ""
	for _, t := range texts {
		if t != "" {
			combined += t + "\n"
		}
	}
	if combined == "" {
		log.Println("[合规] 文本为空，跳过校验")
		return ""
	}
	log.Printf("[合规] 开始校验文本: %s", combined)

	prompt := `你是一个内容安全审核助手。请判断以下用户输入是否包含违规内容。

违规类型包括：色情、赌博、毒品、涉政敏感、暴力、广告推广、个人隐私泄露、无意义内容。

用户输入：
` + combined + `

请只回复 JSON，格式如下：
{"pass": true} 或 {"pass": false, "reason": "违规原因简述"}`

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 100,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", claudeKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[合规] Claude 请求失败: %v", err)
		return ""
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	log.Printf("[合规] Claude 原始响应 status=%d body=%s", resp.StatusCode, string(rawBody))

	if resp.StatusCode != 200 {
		log.Printf("[合规] Claude 返回非200，跳过校验")
		return ""
	}

	var cr claudeResp
	if err := json.Unmarshal(rawBody, &cr); err != nil || len(cr.Content) == 0 {
		log.Printf("[合规] Claude 响应解析失败: %v", err)
		return ""
	}

	var result struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	text := cr.Content[0].Text
	log.Printf("[合规] Claude 响应: %s", text)
	// 提取 JSON
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		json.Unmarshal([]byte(text[start:end+1]), &result)
	}
	if !result.Pass {
		if result.Reason == "" {
			result.Reason = "内容不符合社区规范"
		}
		log.Printf("[合规] 拦截内容，原因: %s", result.Reason)
		return result.Reason
	}
	log.Println("[合规] 内容合规通过")
	return ""
}

// ─── POST /api/report ─────────────────────────────────────────────────────────
func submitReport(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(10 << 20); err != nil {
		c.JSON(400, gin.H{"error": "解析表单失败"})
		return
	}

	lng, _ := strconv.ParseFloat(c.PostForm("lng"), 64)
	lat, _ := strconv.ParseFloat(c.PostForm("lat"), 64)
	address := c.PostForm("address")
	description := c.PostForm("description")
	plateProvince := c.PostForm("plate_province")
	cameraIDStr := c.PostForm("camera_id")

	if lng == 0 || lat == 0 {
		c.JSON(400, gin.H{"error": "缺少坐标信息"})
		return
	}

	// 文本合规校验
	if reason := moderateText(description, plateProvince); reason != "" {
		c.JSON(400, gin.H{"error": "内容审核未通过：" + reason})
		return
	}

	file, header, err := c.Request.FormFile("screenshot")
	if err != nil {
		c.JSON(400, gin.H{"error": "请上传违章截图"})
		return
	}
	defer file.Close()

	// 上传到腐牛图床
	screenshotURL, err := uploadToImgbb(file, header)
	if err != nil {
		c.JSON(500, gin.H{"error": "截图上传失败: " + err.Error()})
		return
	}

	var cameraID uint64
	if cameraIDStr != "" {
		id, _ := strconv.ParseUint(cameraIDStr, 10, 64)
		cameraID = id
	}

	if cameraID == 0 {
		// 查找 50m 内已有点位
		db.QueryRow(`
			SELECT id FROM cameras
			WHERE ABS(lng-?) < 0.0005 AND ABS(lat-?) < 0.0005
			ORDER BY created_at DESC LIMIT 1
		`, lng, lat).Scan(&cameraID)

		if cameraID == 0 {
			res, err := db.Exec(`
				INSERT INTO cameras (lng, lat, address, status, report_count, last_report_at, confidence)
				VALUES (?, ?, ?, 'pending', 1, NOW(), 100)
			`, lng, lat, address)
			if err != nil {
				c.JSON(500, gin.H{"error": "创建点位失败: " + err.Error()})
				return
			}
			id, _ := res.LastInsertId()
			cameraID = uint64(id)
		}
	}

	res, err := db.Exec(`
		INSERT INTO reports (camera_id, screenshot_url, description, plate_province, status, reported_at)
		VALUES (?, ?, ?, ?, 'pending', NOW())
	`, cameraID, screenshotURL, description, plateProvince)
	if err != nil {
		c.JSON(500, gin.H{"error": "上报失败: " + err.Error()})
		return
	}
	reportID, _ := res.LastInsertId()

	go sendFeishuCard(int64(reportID), int64(cameraID), lng, lat, address, screenshotURL, description, plateProvince)

	c.JSON(200, gin.H{"message": "上报成功，等待审核", "report_id": reportID, "camera_id": cameraID})
}

// ─── GET /api/cameras/:id/reports ────────────────────────────────────────────
func getCameraReports(c *gin.Context) {
	rows, err := db.Query(`
		SELECT id, camera_id, screenshot_url, COALESCE(description,''),
		       COALESCE(plate_province,''), status, reported_at
		FROM reports WHERE camera_id=? ORDER BY reported_at DESC LIMIT 20
	`, c.Param("id"))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var reports []Report
	for rows.Next() {
		var r Report
		rows.Scan(&r.ID, &r.CameraID, &r.ScreenshotURL, &r.Description, &r.PlateProvince, &r.Status, &r.ReportedAt)
		reports = append(reports, r)
	}
	if reports == nil {
		reports = []Report{}
	}
	c.JSON(200, reports)
}

// ─── GET /api/cameras/:id/comments ───────────────────────────────────────────
func getComments(c *gin.Context) {
	rows, err := db.Query(`
		SELECT id, camera_id, nickname, content, comment_type, created_at
		FROM comments WHERE camera_id=? ORDER BY created_at DESC LIMIT 50
	`, c.Param("id"))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var comments []Comment
	for rows.Next() {
		var cm Comment
		rows.Scan(&cm.ID, &cm.CameraID, &cm.Nickname, &cm.Content, &cm.CommentType, &cm.CreatedAt)
		comments = append(comments, cm)
	}
	if comments == nil {
		comments = []Comment{}
	}
	c.JSON(200, comments)
}

// ─── POST /api/cameras/:id/comments ──────────────────────────────────────────
func postComment(c *gin.Context) {
	camID := c.Param("id")
	var body struct {
		Nickname    string `json:"nickname"`
		Content     string `json:"content"`
		CommentType string `json:"comment_type"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}
	if body.Content == "" {
		c.JSON(400, gin.H{"error": "内容不能为空"})
		return
	}
	// 文本合规校验
	if reason := moderateText(body.Content, body.Nickname); reason != "" {
		c.JSON(400, gin.H{"error": "内容审核未通过：" + reason})
		return
	}
	if body.Nickname == "" {
		body.Nickname = "匿名"
	}
	validTypes := map[string]bool{"confirm": true, "deny": true, "info": true}
	if !validTypes[body.CommentType] {
		body.CommentType = "info"
	}

	res, err := db.Exec(`
		INSERT INTO comments (camera_id, nickname, content, comment_type, created_at)
		VALUES (?, ?, ?, ?, NOW())
	`, camID, body.Nickname, body.Content, body.CommentType)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	c.JSON(200, gin.H{"id": id, "message": "发送成功"})
}

// ─── GET /api/review/callback ─────────────────────────────────────────────────
func reviewCallback(c *gin.Context) {
	if c.Query("token") != adminToken {
		c.String(403, "无权操作")
		return
	}
	reportID, err := strconv.ParseInt(c.Query("report_id"), 10, 64)
	if err != nil {
		c.String(400, "无效 report_id")
		return
	}
	action := c.Query("action")
	note := c.DefaultQuery("note", "")

	if action == "approve" {
		db.Exec(`UPDATE reports SET status='approved', reviewed_at=NOW() WHERE id=?`, reportID)
		db.Exec(`
			UPDATE cameras SET status='active', confidence=100,
				last_report_at=NOW(), report_count=report_count+1
			WHERE id=(SELECT camera_id FROM reports WHERE id=?)
		`, reportID)
		log.Printf("✅ 审核通过 report_id=%d", reportID)
		c.String(200, fmt.Sprintf("✅ 已通过 report #%d，摄像头已上地图", reportID))
	} else if action == "reject" {
		db.Exec(`UPDATE reports SET status='rejected', reviewer_note=?, reviewed_at=NOW() WHERE id=?`, note, reportID)
		log.Printf("✕ 已拒绝 report_id=%d", reportID)
		c.String(200, fmt.Sprintf("✕ 已拒绝 report #%d", reportID))
	} else {
		c.String(400, "未知操作")
	}
}

// ─── 上传到腐牛图床 (imgbb) ───────────────────────────────────────────────────
func uploadToImgbb(file multipart.File, header *multipart.FileHeader) (string, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fw, err := writer.CreateFormFile("image", header.Filename)
	if err != nil {
		return "", err
	}
	fw.Write(data)
	writer.Close()

	apiKey := imgbbKey
	if apiKey == "" {
		return "", fmt.Errorf("IMGBB_KEY 未配置")
	}

	req, _ := http.NewRequest("POST",
		fmt.Sprintf("https://api.imgbb.com/1/upload?key=%s", apiKey),
		&buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Success {
		return "", fmt.Errorf("imgbb 上传失败")
	}
	return result.Data.URL, nil
}

// ─── 飞书通知 ─────────────────────────────────────────────────────────────────
func sendFeishuCard(reportID, cameraID int64, lng, lat float64, address, screenshotURL, desc, plate string) {
	if feishuWebhook == "" {
		return
	}
	if desc == "" {
		desc = "（无补充说明）"
	}
	if plate == "" {
		plate = "未填写"
	}
	if address == "" {
		address = fmt.Sprintf("%.4f,%.4f", lng, lat)
	}

	approve := fmt.Sprintf("%s/api/review/callback?report_id=%d&action=approve&token=%s", reviewCallbackURL, reportID, adminToken)
	reject  := fmt.Sprintf("%s/api/review/callback?report_id=%d&action=reject&token=%s", reviewCallbackURL, reportID, adminToken)

	// 用纯文本消息，避免卡片格式问题
	text := fmt.Sprintf(
		"📡 新摄像头上报待审核\n\n"+
		"📍 位置：%s\n"+
		"🚗 车牌省份：%s\n"+
		"📝 说明：%s\n"+
		"🖼 截图：%s\n"+
		"🆔 Report:%d  Camera:%d\n\n"+
		"✅ 通过：%s\n"+
		"✕ 拒绝：%s",
		address, plate, desc, screenshotURL,
		reportID, cameraID,
		approve, reject,
	)

	body, _ := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})

	resp, err := http.Post(feishuWebhook, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("飞书推送失败: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("飞书推送成功 report_id=%d, resp=%s", reportID, string(respBody))
}

func larkField(content string, isShort bool) map[string]any {
	return map[string]any{
		"is_short": isShort,
		"text":     map[string]string{"tag": "lark_md", "content": content},
	}
}

func randStr(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	seed := time.Now().UnixNano()
	for i := range b {
		b[i] = chars[seed%int64(len(chars))]
		seed = seed*6364136223846793005 + 1442695040888963407
	}
	return string(b)
}

// suppress unused warning
var _ = context.Background
var _ = strings.TrimSpace
var _ = randStr
