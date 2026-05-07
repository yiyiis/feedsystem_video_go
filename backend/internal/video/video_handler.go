package video

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

type VideoHandler struct {
	service        *VideoService
	accountService *account.AccountService
}

func NewVideoHandler(service *VideoService, accountService *account.AccountService) *VideoHandler {
	return &VideoHandler{service: service, accountService: accountService}
}

func (vh *VideoHandler) PublishVideo(c *gin.Context) {
	var req PublishVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	username, err := jwt.GetUsername(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	video := &Video{
		AuthorID:    authorId,
		Username:    username,
		Title:       req.Title,
		Description: req.Description,
		PlayURL:     req.PlayURL,
		CoverURL:    req.CoverURL,
		CreateTime:  time.Now(),
	}
	if err := vh.service.Publish(c.Request.Context(), video); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, video)
}

const (
	uploadRoot            = ".run/uploads"
	maxVideoUploadSize    = 1 << 30
	maxVideoChunkSize     = 10 << 20
	defaultVideoChunkSize = 5 << 20
)

var uploadIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type initVideoUploadRequest struct {
	FileName string `json:"file_name" binding:"required"`
	FileSize int64  `json:"file_size" binding:"required"`
}

type chunkUploadMeta struct {
	UploadID    string `json:"upload_id"`
	AuthorID    uint   `json:"author_id"`
	FileName    string `json:"file_name"`
	FileSize    int64  `json:"file_size"`
	Ext         string `json:"ext"`
	Date        string `json:"date"`
	TotalChunks int    `json:"total_chunks"`
	CreatedAt   int64  `json:"created_at"`
}

func (vh *VideoHandler) InitVideoUpload(c *gin.Context) {
	authorID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var req initVideoUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.FileSize <= 0 || req.FileSize > maxVideoUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}
	if ext := strings.ToLower(filepath.Ext(req.FileName)); ext != ".mp4" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .mp4 is allowed"})
		return
	}

	uploadID, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload session"})
		return
	}
	meta := chunkUploadMeta{
		UploadID:    uploadID,
		AuthorID:    authorID,
		FileName:    filepath.Base(req.FileName),
		FileSize:    req.FileSize,
		Ext:         ".mp4",
		Date:        time.Now().Format("20060102"),
		TotalChunks: int((req.FileSize + defaultVideoChunkSize - 1) / defaultVideoChunkSize),
		CreatedAt:   time.Now().Unix(),
	}

	dir := chunkSessionDir(authorID, uploadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := writeChunkMeta(dir, meta); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":    uploadID,
		"chunk_size":   defaultVideoChunkSize,
		"total_chunks": meta.TotalChunks,
	})
}

func (vh *VideoHandler) UploadVideoChunk(c *gin.Context) {
	authorID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	uploadID := c.PostForm("upload_id")
	if !uploadIDPattern.MatchString(uploadID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid upload_id"})
		return
	}
	chunkIndex, err := strconv.Atoi(c.PostForm("chunk_index"))
	if err != nil || chunkIndex < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk_index"})
		return
	}

	dir := chunkSessionDir(authorID, uploadID)
	meta, err := readChunkMeta(dir)
	if err != nil || meta.AuthorID != authorID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload session not found"})
		return
	}
	if chunkIndex >= meta.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk_index out of range"})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing chunk file"})
		return
	}
	if f.Size <= 0 || f.Size > maxVideoChunkSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk size"})
		return
	}

	if err := c.SaveUploadedFile(f, chunkPath(dir, chunkIndex)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"upload_id": uploadID, "chunk_index": chunkIndex, "uploaded": true})
}

func (vh *VideoHandler) CompleteVideoUpload(c *gin.Context) {
	authorID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !uploadIDPattern.MatchString(req.UploadID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid upload_id"})
		return
	}

	dir := chunkSessionDir(authorID, req.UploadID)
	meta, err := readChunkMeta(dir)
	if err != nil || meta.AuthorID != authorID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload session not found"})
		return
	}

	var uploadedSize int64
	for i := 0; i < meta.TotalChunks; i++ {
		st, err := os.Stat(chunkPath(dir, i))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("missing chunk %d", i)})
			return
		}
		uploadedSize += st.Size()
	}
	if uploadedSize != meta.FileSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "uploaded chunks size mismatch"})
		return
	}

	relDir := filepath.Join("videos", fmt.Sprintf("%d", authorID), meta.Date)
	absDir := filepath.Join(uploadRoot, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	filename += meta.Ext
	absPath := filepath.Join(absDir, filename)
	if err := mergeChunks(dir, absPath, meta.TotalChunks); err != nil {
		_ = os.Remove(absPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = os.RemoveAll(dir)

	urlPath := path.Join("/static", "videos", fmt.Sprintf("%d", authorID), meta.Date, filename)
	c.JSON(http.StatusOK, gin.H{"url": buildAbsoluteURL(c, urlPath), "play_url": buildAbsoluteURL(c, urlPath)})
}

func chunkSessionDir(authorID uint, uploadID string) string {
	return filepath.Join(uploadRoot, "chunks", fmt.Sprintf("%d", authorID), uploadID)
}

func chunkPath(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf("chunk_%06d.part", index))
}

func writeChunkMeta(dir string, meta chunkUploadMeta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "metadata.json"), b, 0o644)
}

func readChunkMeta(dir string) (chunkUploadMeta, error) {
	var meta chunkUploadMeta
	b, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return meta, err
	}
	return meta, json.Unmarshal(b, &meta)
}

func mergeChunks(dir, dst string, totalChunks int) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	for i := 0; i < totalChunks; i++ {
		in, err := os.Open(chunkPath(dir, i))
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (vh *VideoHandler) UploadVideo(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	const maxSize = 200 << 20
	if f.Size <= 0 || f.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	if ext != ".mp4" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .mp4 is allowed"})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", authorId), date)
	root := uploadRoot
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	urlPath := path.Join("/static", "videos", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":      buildAbsoluteURL(c, urlPath),
		"play_url": buildAbsoluteURL(c, urlPath),
	})
}

func (vh *VideoHandler) UploadCover(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	const maxSize = 10 << 20
	if f.Size <= 0 || f.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp is allowed"})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("covers", fmt.Sprintf("%d", authorId), date)
	root := uploadRoot
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	urlPath := path.Join("/static", "covers", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":       buildAbsoluteURL(c, urlPath),
		"cover_url": buildAbsoluteURL(c, urlPath),
	})
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func buildAbsoluteURL(c *gin.Context, p string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, p)
}

func (vh *VideoHandler) DeleteVideo(c *gin.Context) {
	var req DeleteVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if err := vh.service.Delete(c.Request.Context(), req.ID, authorId); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "video deleted"})
}

func (vh *VideoHandler) ListByAuthorID(c *gin.Context) {
	var req ListByAuthorIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	videos, err := vh.service.ListByAuthorID(c.Request.Context(), req.AuthorID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if videos == nil {
		videos = []Video{}
	}
	c.JSON(200, videos)
}

func (vh *VideoHandler) GetDetail(c *gin.Context) {
	var req GetDetailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	video, err := vh.service.GetDetail(c.Request.Context(), req.ID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, video)
}

func (vh *VideoHandler) UpdateLikesCount(c *gin.Context) {
	var req UpdateLikesCountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if err := vh.service.UpdateLikesCount(c.Request.Context(), req.ID, req.LikesCount); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "likes count updated"})
}
