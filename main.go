package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/net/html"
)

const (
	DeepSeekAPIURL  = "https://api.deepseek.com/v1/translate"
	CacheDuration   = 24 * time.Hour
	MaxTextLength   = 5000
	DefaultLanguage = "en"
	RequestTimeout  = 15 * time.Second
)

var (
	translationCache sync.Map
	cacheMutex       sync.RWMutex
	apiKey           = os.Getenv("DEEPSEEK_API_KEY")
)

type CacheEntry struct {
	Content    string
	Expiration time.Time
}

func main() {
	app := fiber.New(fiber.Config{
		Prefork:       true,
		CaseSensitive: true,
	})

	// Middleware to detect language
	app.Use(func(c *fiber.Ctx) error {
		lang := DefaultLanguage

		if l := c.Query("lang"); l != "" {
			lang = l
		} else {
			// Detect browser language
			acceptLang := c.Get("Accept-Language")
			lang = detectPreferredLanguage(acceptLang)
		}

		c.Locals("lang", lang)
		return c.Next()
	})

	// Route for explicit translation API
	app.Get("/api/translate", handleTranslateAPI)

	// Route for automatic translation proxy
	app.Get("/*", handleAutomaticTranslation)

	log.Println("Translation server started on port :8080")
	app.Listen(":3080")
}

func handleTranslateAPI(c *fiber.Ctx) error {
	url := c.Query("url")
	lang := c.Locals("lang").(string)

	if url == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "URL is required",
		})
	}

	if cached, ok := getFromCache(url, lang); ok {
		return c.JSON(fiber.Map{
			"cached":     true,
			"original":   cached,
			"translated": cached,
			"language":   lang,
		})
	}

	content, err := extractContentFromURL(url)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to extract content: " + err.Error(),
		})
	}

	translated, err := translateWithDeepSeek(content, lang)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "Translation service unavailable: " + err.Error(),
		})
	}

	addToCache(url, lang, translated)

	return c.JSON(fiber.Map{
		"cached":     false,
		"original":   content,
		"translated": translated,
		"language":   lang,
	})
}

func handleAutomaticTranslation(c *fiber.Ctx) error {
	lang := c.Locals("lang").(string)
	requestedURL := c.OriginalURL()

	if strings.HasPrefix(requestedURL, "/api/") || isStaticFile(requestedURL) {
		return c.Next()
	}

	if lang == DefaultLanguage {
		return c.Next()
	}

	if cached, ok := getFromCache(requestedURL, lang); ok {
		return c.SendString(cached)
	}

	err := c.Next()
	if err != nil {
		return err
	}

	contentType := c.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		return nil
	}

	body := c.Response().Body()

	translatedHTML, err := processHTML(string(body), lang)
	if err != nil {
		log.Printf("Error processing HTML: %v", err)
		return c.SendString(string(body))
	}

	addToCache(requestedURL, lang, translatedHTML)

	return c.SendString(translatedHTML)
}

func processHTML(htmlContent string, lang string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("error parsing HTML: %w", err)
	}

	var processNode func(*html.Node)
	processNode = func(n *html.Node) {
		if n.Type == html.TextNode && strings.TrimSpace(n.Data) != "" {
			translated, err := translateWithDeepSeek(n.Data, lang)
			if err == nil {
				n.Data = translated
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			processNode(c)
		}
	}

	processNode(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return "", fmt.Errorf("error rendering HTML: %w", err)
	}

	return buf.String(), nil
}

func translateWithDeepSeek(text string, targetLang string) (string, error) {

	if len(strings.TrimSpace(text)) == 0 {
		return text, nil
	}

	cacheKey := "text_" + targetLang + "_" + hashText(text)
	if cached, ok := getFromCache(cacheKey, ""); ok {
		return cached, nil
	}

	if len(text) > MaxTextLength {
		text = text[:MaxTextLength]
	}

	translated, err := callDeepSeekAPI(text, targetLang)
	if err != nil {
		return "", fmt.Errorf("translation error: %w", err)
	}

	addToCache(cacheKey, "", translated)
	return translated, nil
}

func callDeepSeekAPI(text, targetLang string) (string, error) {
	if apiKey == "" {
		return "", errors.New("API key not configured")
	}

	payload := map[string]interface{}{
		"text":        text,
		"target_lang": targetLang,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error serializing payload: %w", err)
	}

	req, err := http.NewRequest("POST", DeepSeekAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: RequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error: %s - %s", resp.Status, string(body))
	}

	var result struct {
		Data struct {
			Translations []struct {
				Text string `json:"text"`
			} `json:"translations"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("error decoding response: %w", err)
	}

	if len(result.Data.Translations) == 0 {
		return "", errors.New("no translations returned")
	}

	return result.Data.Translations[0].Text, nil
}

func extractContentFromURL(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("error accessing URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("non-OK status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body: %w", err)
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("error parsing HTML: %w", err)
	}

	return extractText(doc), nil
}

func extractText(n *html.Node) string {
	var sb strings.Builder
	var f func(*html.Node)

	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				sb.WriteString(text + " ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}

	f(n)
	return strings.TrimSpace(sb.String())
}

func detectPreferredLanguage(header string) string {
	supported := map[string]bool{
		"en": true, "pt": true, "es": true, "fr": true,
		"de": true, "it": true, "ja": true, "zh": true, "ru": true,
	}

	parts := strings.Split(header, ",")
	for _, part := range parts {
		lang := strings.ToLower(strings.TrimSpace(strings.Split(part, ";")[0]))
		if len(lang) > 2 {
			lang = lang[:2]
		}
		if supported[lang] {
			return lang
		}
	}
	return DefaultLanguage
}

func addToCache(key, lang, content string) {
	cacheKey := key
	if lang != "" {
		cacheKey += "|" + lang
	}

	cacheMutex.Lock()
	translationCache.Store(cacheKey, CacheEntry{
		Content:    content,
		Expiration: time.Now().Add(CacheDuration),
	})
	cacheMutex.Unlock()
}

func getFromCache(key, lang string) (string, bool) {
	cacheKey := key
	if lang != "" {
		cacheKey += "|" + lang
	}

	cacheMutex.RLock()
	defer cacheMutex.RUnlock()

	if entry, ok := translationCache.Load(cacheKey); ok {
		cached := entry.(CacheEntry)
		if time.Now().Before(cached.Expiration) {
			return cached.Content, true
		}
	}
	return "", false
}

func hashText(text string) string {

	return fmt.Sprintf("%d", len(text))
}

func isStaticFile(path string) bool {
	extensions := []string{".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".woff", ".woff2"}
	for _, ext := range extensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}
