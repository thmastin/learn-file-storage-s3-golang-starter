package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse videoID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couludn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	const maxMemory = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't retrive video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized to retrive video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to parse from file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusExpectationFailed, "File is not of type mp4", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temporary file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to copy file to temp file", err)
		return
	}

	newPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process video for faststart", err)
		return
	}

	processedFile, err := os.Open(newPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open processedFile", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	_, err = processedFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to set to start of processedFile", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get aspect ratio of video", err)
		return
	}

	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	case "other":
		prefix = "other/"
	}

	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate random bytes", err)
		return
	}

	fileName := hex.EncodeToString(randomBytes)
	key := prefix + fileName + ".mp4"

	putObject := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        processedFile,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), putObject)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video to s3", err)
		return
	}

	new_url := cfg.s3Bucket + "," + key
	video.VideoURL = &new_url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video in database", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate presigned url", err)
		return
	}

	log.Printf("Successfully uploaded video %s to S3 with URL: %s", videoID, *video.VideoURL)
	respondWithJSON(w, http.StatusOK, video)
	return
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	err = json.Unmarshal(out.Bytes(), &output)
	if err != nil {
		return "", err
	}
	if len(output.Streams) < 1 {
		return "", fmt.Errorf("no stream data")
	}
	height := output.Streams[0].Height
	width := output.Streams[0].Width

	ratio := float64(width) / float64(height)

	if ratio >= 1.7 && ratio <= 1.8 { // roughly 16:9 (1.777...)
		return "16:9", nil
	} else if ratio >= 0.5 && ratio <= 0.6 { // roughly 9:16 (0.5625)
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filepath string) (string, error) {
	output := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return output, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s3Client)

	object := s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}

	presignedKey, err := presigner.PresignGetObject(context.Background(), &object, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return presignedKey.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	splitURL := strings.Split(*video.VideoURL, ",")
	if len(splitURL) < 2 {
		return database.Video{}, fmt.Errorf("invalid video URL format")
	}

	bucket := splitURL[0]
	key := splitURL[1]

	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return database.Video{}, err
	}

	video.VideoURL = &presignedURL

	return video, nil
}
