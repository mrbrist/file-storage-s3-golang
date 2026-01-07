package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxSize int64 = 1 << 30 // 1GB
	http.MaxBytesReader(w, r.Body, maxSize)
	defer r.Body.Close()

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video for", videoID, "by user", userID)
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not own this video", nil)
		return
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusNotFound, "No video found", err)
		return
	}
	defer file.Close()

	mtype, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "COuld not parse video", err)
		return
	}
	if mtype != "video/mp4" {
		respondWithError(w, http.StatusInternalServerError, "File is not an mp4", nil)
		return
	}

	tmp_file, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create temp file", err)
		return
	}
	defer os.Remove(tmp_file.Name())
	defer tmp_file.Close()

	io.Copy(tmp_file, file)
	tmp_file.Seek(0, io.SeekStart)

	prefix, err := getVideoAspectRatio(tmp_file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get aspect ratio for video", err)
		return
	}

	switch prefix {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	case "other":
		prefix = "other/"
	}

	r32 := make([]byte, 32)
	rand.Read(r32)
	rand_url := prefix + base64.RawURLEncoding.EncodeToString(r32) + ".mp4"

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &rand_url,
		Body:        tmp_file,
		ContentType: &mtype,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not put object into s3", err)
		return
	}

	url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, rand_url)
	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error updating video", err)
		return
	}
}

func getVideoAspectRatio(filePath string) (string, error) {
	type Stream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type Output struct {
		Streams []Stream `json:"streams"`
	}

	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	var buf bytes.Buffer
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe failed: %w", err)
	}

	var output Output
	if err := json.Unmarshal(buf.Bytes(), &output); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	ratio := "other"
	w := output.Streams[0].Width
	h := output.Streams[0].Height

	if w > 0 && h > 0 {
		ar := float64(w) / float64(h)

		const eps = 0.02

		if almostEqual(ar, 16.0/9.0, eps) {
			ratio = "16:9"
		} else if almostEqual(ar, 9.0/16.0, eps) {
			ratio = "9:16"
		}
	}

	return ratio, nil
}

// checks to see if the numbers are equal to within a given tolerence
func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}
