package main

import (
	"bytes"
	"context"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	db                *pgxpool.Pool
	supabaseURL       = getEnv("SUPABASE_URL", "")
	supabaseKey       = getEnv("SUPABASE_SERVICE_KEY", "")
	feishuWebhook     = getEnv("FEISHU_WEBHOOK", "")
	reviewCallbackURL = getEnv("REVIEW_CALLBACK_URL", "http://localhost:8080")
	adminToken        = getEnv("ADMIN_TOKEN", "changeme")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}

type Camera struct {
	ID           int64     `json:"id"`
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
	ID            int64     `json:"id"`
	CameraID      int64     `json:"camera_id"`
	ScreenshotURL string    `json:"screenshot_url"`
	Description   string    `json:"description"`
	PlateProvince string    `json:"plate_province"`
	Status        string    `json:"status"`
	ReviewerNote  string    `json:"reviewer_note"`
	ReportedAt    time.Time `json:"reported_at"`
}

func main() {
	dbURL := getEnv("DATABASE_URL", "")
	if dbURL == "" { log.Fatal("DATABASE_URL is required") }

	var err error
	db, err = pgxpool.New(context.Background(), dbURL)
	if err != nil { log.Fatalf("DB connect failed: %v", err) }
	defer db.Close()

	if err = db.Ping(context.Background()); err != nil { log.Fatalf("DB ping failed: %v", err) }
	log.Println("DB connected")

	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "Authorization"},
	}))

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.GET("/api/cameras", getCameras)
	r.POST("/api/report", submitReport)
	r.GET("/api/cameras/:id/reports", getCameraReports)
	r.GET("/api/review/callback", reviewCallback)

	port := getEnv("PORT", "8080")
	log.Printf("Server on :%s", port)
	r.Run(":" + port)
}

func getCameras(c *gin.Context) {
	rows, err := db.Query(context.Background(), `
		SELECT id, lng, lat, address, status, report_count, last_report_at, confidence, created_at
		FROM cameras WHERE status IN ('active','inactive') ORDER BY created_at DESC
	`)
	if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
	defer rows.Close()

	var cameras []Camera
	for rows.Next() {
		var cam Camera
		if err := rows.Scan(&cam.ID, &cam.Lng, &cam.Lat, &cam.Address,
			&cam.Status, &cam.ReportCount, &cam.LastReportAt, &cam.Confidence, &cam.CreatedAt); err != nil {
			continue
		}
		days := time.Since(cam.LastReportAt).Hours() / 24
		cam.Confidence = math.Round(cam.Confidence*math.Pow(0.5, days/180)*10) / 10
		if cam.Confidence < 0 { cam.Confidence = 0 }
		if cam.Confidence < 20 && cam.Status == "active" { cam.Status = "inactive" }
		cam.UpdateTime = cam.LastReportAt.Format("2006-01-02")
		cameras = append(cameras, cam)
	}
	if cameras == nil { cameras = []Camera{} }
	c.JSON(200, cameras)
}

func submitReport(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(10 << 20); err != nil {
		c.JSON(400, gin.H{"error": "解析表单失败"}); return
	}
	lng, _ := strconv.ParseFloat(c.PostForm("lng"), 64)
	lat, _ := strconv.ParseFloat(c.PostForm("lat"), 64)
	address := c.PostForm("address")
	description := c.PostForm("description")
	plateProvince := c.PostForm("plate_province")
	cameraIDStr := c.PostForm("camera_id")

	if lng == 0 || lat == 0 { c.JSON(400, gin.H{"error": "缺少坐标信息"}); return }

	file, header, err := c.Request.FormFile("screenshot")
	if err != nil { c.JSON(400, gin.H{"error": "请上传违章截图"}); return }
	defer file.Close()

	screenshotURL, err := uploadToSupabase(file, header)
	if err != nil { c.JSON(500, gin.H{"error": "截图上传失败: " + err.Error()}); return }

	ctx := context.Background()
	var cameraID int64
	if cameraIDStr != "" { cameraID, _ = strconv.ParseInt(cameraIDStr, 10, 64) }

	if cameraID == 0 {
		_ = db.QueryRow(ctx, `
			SELECT id FROM cameras WHERE ABS(lng-$1)<0.0005 AND ABS(lat-$2)<0.0005
			ORDER BY created_at DESC LIMIT 1
		`, lng, lat).Scan(&cameraID)

		if cameraID == 0 {
			err = db.QueryRow(ctx, `
				INSERT INTO cameras (lng,lat,address,status,report_count,last_report_at,confidence)
				VALUES ($1,$2,$3,'pending',1,NOW(),100) RETURNING id
			`, lng, lat, address).Scan(&cameraID)
			if err != nil { c.JSON(500, gin.H{"error": "创建点位失败: " + err.Error()}); return }
		}
	}

	var reportID int64
	err = db.QueryRow(ctx, `
		INSERT INTO reports (camera_id,screenshot_url,description,plate_province,status,reported_at)
		VALUES ($1,$2,$3,$4,'pending',NOW()) RETURNING id
	`, cameraID, screenshotURL, description, plateProvince).Scan(&reportID)
	if err != nil { c.JSON(500, gin.H{"error": "上报失败: " + err.Error()}); return }

	go sendFeishuCard(reportID, cameraID, lng, lat, address, screenshotURL, description, plateProvince)

	c.JSON(200, gin.H{"message": "上报成功，等待审核", "report_id": reportID, "camera_id": cameraID})
}

func getCameraReports(c *gin.Context) {
	rows, err := db.Query(context.Background(), `
		SELECT id, camera_id, screenshot_url, description, plate_province, status, reported_at
		FROM reports WHERE camera_id=$1 ORDER BY reported_at DESC LIMIT 20
	`, c.Param("id"))
	if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
	defer rows.Close()
	var reports []Report
	for rows.Next() {
		var r Report
		rows.Scan(&r.ID, &r.CameraID, &r.ScreenshotURL, &r.Description, &r.PlateProvince, &r.Status, &r.ReportedAt)
		reports = append(reports, r)
	}
	if reports == nil { reports = []Report{} }
	c.JSON(200, reports)
}

func reviewCallback(c *gin.Context) {
	if c.Query("token") != adminToken { c.String(403, "无权操作"); return }

	reportID, err := strconv.ParseInt(c.Query("report_id"), 10, 64)
	if err != nil { c.String(400, "无效 report_id"); return }

	action := c.Query("action")
	note := c.DefaultQuery("note", "")
	ctx := context.Background()

	if action == "approve" {
		db.Exec(ctx, `UPDATE reports SET status='approved',reviewed_at=NOW() WHERE id=$1`, reportID)
		db.Exec(ctx, `
			UPDATE cameras SET status='active',confidence=100,last_report_at=NOW(),report_count=report_count+1
			WHERE id=(SELECT camera_id FROM reports WHERE id=$1)
		`, reportID)
		log.Printf("审核通过 report_id=%d", reportID)
		c.String(200, fmt.Sprintf("✅ 已通过 report #%d，摄像头已激活上地图", reportID))
	} else if action == "reject" {
		db.Exec(ctx, `UPDATE reports SET status='rejected',reviewer_note=$2,reviewed_at=NOW() WHERE id=$1`, reportID, note)
		log.Printf("已拒绝 report_id=%d", reportID)
		c.String(200, fmt.Sprintf("✕ 已拒绝 report #%d", reportID))
	} else {
		c.String(400, "未知操作")
	}
}

func uploadToSupabase(file multipart.File, header *multipart.FileHeader) (string, error) {
	data, err := io.ReadAll(file)
	if err != nil { return "", err }

	ext := "jpg"
	if parts := strings.Split(header.Filename, "."); len(parts) > 1 { ext = parts[len(parts)-1] }
	filename := fmt.Sprintf("%d_%s.%s", time.Now().UnixMilli(), randStr(8), ext)

	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/storage/v1/object/screenshots/%s", supabaseURL, filename),
		bytes.NewReader(data))
	req.Header.Set("apikey", supabaseKey)
	req.Header.Set("Authorization", "Bearer "+supabaseKey)
	req.Header.Set("Content-Type", header.Header.Get("Content-Type"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("storage %d: %s", resp.StatusCode, body)
	}
	return fmt.Sprintf("%s/storage/v1/object/public/screenshots/%s", supabaseURL, filename), nil
}

func sendFeishuCard(reportID, cameraID int64, lng, lat float64, address, screenshotURL, desc, plate string) {
	if feishuWebhook == "" { return }
	if desc == "" { desc = "（无补充说明）" }
	if plate == "" { plate = "未填写" }
	if address == "" { address = fmt.Sprintf("%.4f,%.4f", lng, lat) }

	approve := fmt.Sprintf("%s/api/review/callback?report_id=%d&action=approve&token=%s", reviewCallbackURL, reportID, adminToken)
	reject  := fmt.Sprintf("%s/api/review/callback?report_id=%d&action=reject&token=%s", reviewCallbackURL, reportID, adminToken)

	card := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": "📡 新摄像头上报待审核"},
				"template": "orange",
			},
			"elements": []any{
				map[string]any{
					"tag": "div",
					"fields": []any{
						larkField("**位置**\n"+address, true),
						larkField("**车牌省份**\n"+plate, true),
						larkField("**说明**\n"+desc, false),
						larkField(fmt.Sprintf("**ID** Report:%d  Camera:%d", reportID, cameraID), false),
					},
				},
				map[string]any{"tag": "img", "img_key": screenshotURL,
					"alt": map[string]string{"tag": "plain_text", "content": "违章截图"}},
				map[string]any{"tag": "hr"},
				map[string]any{
					"tag": "action",
					"actions": []any{
						map[string]any{"tag": "button", "type": "primary", "url": approve,
							"text": map[string]string{"tag": "plain_text", "content": "✅ 通过"}},
						map[string]any{"tag": "button", "type": "danger", "url": reject,
							"text": map[string]string{"tag": "plain_text", "content": "✕ 拒绝"}},
					},
				},
			},
		},
	}

	body, _ := json.Marshal(card)
	resp, err := http.Post(feishuWebhook, "application/json", bytes.NewReader(body))
	if err != nil { log.Printf("飞书推送失败: %v", err); return }
	defer resp.Body.Close()
	log.Printf("飞书推送成功 report_id=%d", reportID)
}

func larkField(content string, isShort bool) map[string]any {
	return map[string]any{"is_short": isShort, "text": map[string]string{"tag": "lark_md", "content": content}}
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
