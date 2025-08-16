package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/syslog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

// Конфиг скрипта
type Config struct {
	ZabbixAPIURL      string
	APIToken          string
	CheckInterval     time.Duration
	OffDuration       time.Duration
	MediaNames        []string
	StateFile         string
	MattermostWebhook string
}

type ZabbixRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	Auth    string      `json:"auth"`
	ID      int         `json:"id"`
}

type MediaType struct {
	MediaTypeID string `json:"mediatypeid"`
	Name        string `json:"name"`
	Status      string `json:"status"`
}

type MediaState map[string]time.Time

type UserGroup struct {
	ID    string   `json:"usrgrpid"`
	Name  string   `json:"name"`
	Users []string `json:"users"`
}
type GroupState map[string]UserGroup

const groupStateFilename = "usergroup_state.json"

func main() {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)

	sysLogger, err := syslog.New(syslog.LOG_INFO|syslog.LOG_LOCAL0, "zabbix-media-watcher")
	if err != nil {
		logger.Warnf("Не удалось подключиться к syslog: %v", err)
		sysLogger = nil
	}

	logger.Info("Сервис мониторинга медиа Zabbix запущен")

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	logger.WithFields(logrus.Fields{
		"api_url":         cfg.ZabbixAPIURL,
		"check_interval":  cfg.CheckInterval,
		"off_duration":    cfg.OffDuration,
		"media_names":     cfg.MediaNames,
		"mm_webhook_used": cfg.MattermostWebhook != "",
	}).Info("Конфигурация загружена")

	state, err := loadState(cfg.StateFile)
	if err != nil {
		logger.Warnf("Ошибка загрузки состояния: %v", err)
		state = make(MediaState)
	} else {
		logger.Infof("Состояние загружено: %d записей", len(state))
	}

	groupState, groupStateExisted, err := loadGroupState(groupStateFilename)
	if err != nil {
		logger.Warnf("Ошибка загрузки состояния групп: %v", err)
		groupState = make(GroupState)
		groupStateExisted = false
	} else {
		if groupStateExisted {
			logger.Infof("Состояние групп загружено: %d записей", len(groupState))
		} else {
			logger.Infof("Файл состояния групп не найден — при первой проверке будет создан baseline (уведомлений не будет)")
		}
	}

	for {
		logger.Info("Начало цикла проверки медиа-типов")
		processMediaTypes(cfg, state, logger, sysLogger)

		baselineMode := !groupStateExisted
		processUserGroups(cfg, groupState, logger, sysLogger, baselineMode)

		if baselineMode {
			groupStateExisted = true
		}

		logger.Infof("Ожидание следующей проверки через %v", cfg.CheckInterval)
		time.Sleep(cfg.CheckInterval)
	}
}

func loadConfig() (*Config, error) {
	_ = godotenv.Load()

	checkInterval, err := strconv.Atoi(os.Getenv("MEDIA_CHECK_INTERVAL"))
	if err != nil {
		return nil, fmt.Errorf("неверный формат MEDIA_CHECK_INTERVAL: %v", err)
	}

	offDuration, err := strconv.Atoi(os.Getenv("MEDIA_OFF_DURATION"))
	if err != nil {
		return nil, fmt.Errorf("неверный формат MEDIA_OFF_DURATION: %v", err)
	}

	mediaNames := []string{}
	if s := strings.TrimSpace(os.Getenv("MEDIA_NAMES")); s != "" {
		for _, p := range strings.Split(s, ",") {
			mediaNames = append(mediaNames, strings.TrimSpace(p))
		}
	}

	return &Config{
		ZabbixAPIURL:      strings.TrimRight(os.Getenv("ZABBIX_API_URL"), "/"),
		APIToken:          os.Getenv("ZABBIX_API_TOKEN"),
		CheckInterval:     time.Duration(checkInterval) * time.Minute,
		OffDuration:       time.Duration(offDuration) * time.Minute,
		MediaNames:        mediaNames,
		StateFile:         "media_state.json",
		MattermostWebhook: strings.TrimSpace(os.Getenv("MM_WEBHOOK_URL")),
	}, nil
}

func loadState(filename string) (MediaState, error) {
	state := make(MediaState)
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

func saveState(filename string, state MediaState, logger *logrus.Logger) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filename, data, 0644); err != nil {
		return err
	}
	logger.Infof("Состояние сохранено в %s", filename)
	return nil
}

func processMediaTypes(cfg *Config, state MediaState, logger *logrus.Logger, sysLogger *syslog.Writer) {
	mediaTypes, err := getMediaTypes(cfg, logger)
	if err != nil {
		logger.Errorf("Ошибка получения медиа-типов: %v", err)
		return
	}
	if len(mediaTypes) == 0 {
		logger.Warning("Не получено ни одного медиа-типа для обработки")
		return
	}
	currentTime := time.Now()
	stateChanged := false
	foundDisabled := false
	for _, media := range mediaTypes {
		logEntry := logger.WithFields(logrus.Fields{
			"media_id":   media.MediaTypeID,
			"media_name": media.Name,
			"status":     media.Status,
		})
		logEntry.Info("Проверка медиа")
		if media.Status == "1" {
			foundDisabled = true
			firstSeen, exists := state[media.MediaTypeID]
			if !exists {
				state[media.MediaTypeID] = currentTime
				stateChanged = true
				logEntry.WithField("action", "state_recorded").Warn("Обнаружено отключённое медиа")
				if sysLogger != nil {
					_ = sysLogger.Warning(fmt.Sprintf("Обнаружено выключенное media: id=%s name=%s", media.MediaTypeID, media.Name))
				}
				if cfg.MattermostWebhook != "" {
					remaining := cfg.OffDuration - time.Since(currentTime)
					msg := fmt.Sprintf("Обнаружено отключенное медиа: %s\nБудет автоматически включено через: %s",
						media.Name, remaining.Round(time.Minute))
					sendMattermostNotification(cfg, msg, logger)
				}
			} else {
				disabledDuration := currentTime.Sub(firstSeen)
				logEntry = logEntry.WithField("disabled_duration", disabledDuration.Round(time.Second))
				if disabledDuration >= cfg.OffDuration {
					logEntry.Warn("Медиа отключено дольше разрешённого времени")
					if sysLogger != nil {
						_ = sysLogger.Warning(fmt.Sprintf("Media id=%s name=%s отключено %v — превышен порог %v", media.MediaTypeID, media.Name, disabledDuration.Round(time.Second), cfg.OffDuration))
					}

					err := enableMediaType(cfg, media.MediaTypeID, logger)
					if err != nil {
						logEntry.WithError(err).Error("Ошибка включения медиа")
						if cfg.MattermostWebhook != "" {
							msg := fmt.Sprintf("Ошибка включения медиа: %s\nОшибка: %v", media.Name, err)
							sendMattermostNotification(cfg, msg, logger)
						}
					} else {
						logEntry.Info("Медиа успешно включено")
						if sysLogger != nil {
							_ = sysLogger.Info(fmt.Sprintf("Скрипт включил media id=%s name=%s", media.MediaTypeID, media.Name))
						}
						if cfg.MattermostWebhook != "" {
							msg := fmt.Sprintf("Медиа %s было автоматически включено скриптом.", media.Name)
							sendMattermostNotification(cfg, msg, logger)
						}
						delete(state, media.MediaTypeID)
						stateChanged = true
					}
				} else {
					logEntry.Info("Медиа отключено, но ещё не превышен лимит времени")
					if cfg.MattermostWebhook != "" && time.Since(firstSeen).Minutes() >= 30 {
						remaining := cfg.OffDuration - disabledDuration
						msg := fmt.Sprintf("Медиа отключено: %s\nОтключено: %s назад\nАвтоматическое включение через: %s",
							media.Name, disabledDuration.Round(time.Minute), remaining.Round(time.Minute))
						sendMattermostNotification(cfg, msg, logger)
					}
				}
			}
		} else if _, exists := state[media.MediaTypeID]; exists {
			delete(state, media.MediaTypeID)
			stateChanged = true
			logEntry.Info("Медиа включено - удалено из состояния")
			if cfg.MattermostWebhook != "" {
				msg := fmt.Sprintf("Медиа восстановлено: %s", media.Name)
				sendMattermostNotification(cfg, msg, logger)
			}
		}
	}
	if !foundDisabled {
		logger.Info("Все отслеживаемые медиа включены")
	}
	if stateChanged {
		if err := saveState(cfg.StateFile, state, logger); err != nil {
			logger.Errorf("Ошибка сохранения состояния: %v", err)
		}
	}
}

func getMediaTypes(cfg *Config, logger *logrus.Logger) ([]MediaType, error) {
	requestBody := ZabbixRequest{
		JSONRPC: "2.0",
		Method:  "mediatype.get",
		Params: map[string]interface{}{
			"output": []string{"mediatypeid", "name", "status"},
			"filter": map[string]interface{}{
				"name": cfg.MediaNames,
			},
		},
		Auth: cfg.APIToken,
		ID:   1,
	}
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(cfg.ZabbixAPIURL+"/api_jsonrpc.php", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var response struct {
		Result []MediaType `json:"result"`
		Error  struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err = json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Error.Code != 0 {
		return nil, fmt.Errorf("ошибка API (%d): %s - %s", response.Error.Code, response.Error.Message, response.Error.Data)
	}
	logger.Infof("Получено %d медиа-типов", len(response.Result))
	return response.Result, nil
}

func enableMediaType(cfg *Config, mediaTypeID string, logger *logrus.Logger) error {
	requestBody := ZabbixRequest{
		JSONRPC: "2.0",
		Method:  "mediatype.update",
		Params: map[string]interface{}{
			"mediatypeid": mediaTypeID,
			"status":      "0",
		},
		Auth: cfg.APIToken,
		ID:   2,
	}
	jsonData, _ := json.Marshal(requestBody)
	resp, err := http.Post(cfg.ZabbixAPIURL+"/api_jsonrpc.php", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var response struct {
		Result struct {
			MediaTypeIDs []string `json:"mediatypeids"`
		} `json:"result"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err = json.Unmarshal(body, &response); err != nil {
		return err
	}
	if response.Error.Code != 0 {
		return fmt.Errorf("ошибка API (%d): %s - %s", response.Error.Code, response.Error.Message, response.Error.Data)
	}
	return nil
}

func sendMattermostNotification(cfg *Config, message string, logger *logrus.Logger) {
	if cfg.MattermostWebhook == "" {
		logger.Warn("Mattermost Webhook URL не задан, уведомление не отправлено")
		return
	}
	payload := map[string]string{"text": message}
	data, _ := json.Marshal(payload)
	resp, err := http.Post(cfg.MattermostWebhook, "application/json", bytes.NewBuffer(data))
	if err != nil {
		logger.WithError(err).Error("Ошибка отправки уведомления в Mattermost")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Errorf("Ошибка отправки уведомления в Mattermost: %s", string(body))
	}
}

// ---------------- Мониторинг UserGroup----------------

func loadGroupState(filename string) (GroupState, bool, error) {
	state := make(GroupState)
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if info.Size() == 0 {
		return state, true, nil
	}
	file, err := os.Open(filename)
	if err != nil {
		return state, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		return state, true, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, true, err
	}
	return state, true, nil
}

func saveGroupState(filename string, state GroupState, logger *logrus.Logger) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filename, data, 0644); err != nil {
		return err
	}
	logger.Infof("Состояние групп сохранено в %s", filename)
	return nil
}

func processUserGroups(cfg *Config, prev GroupState, logger *logrus.Logger, sysLogger *syslog.Writer, baselineMode bool) {
	current, err := getUserGroups(cfg, logger)
	if err != nil {
		logger.Errorf("Ошибка получения групп пользователей: %v", err)
		return
	}

	// При первом запуске сохраняем и НЕ шлём уведомлений. А то засрёт весь канал в ММ
	if baselineMode {
		if err := saveGroupState(groupStateFilename, current, logger); err != nil {
			logger.Errorf("Не удалось сохранить baseline групп: %v", err)
		} else {
			logger.Infof("Baseline групп сохранён в %s — уведомлений не отправлено", groupStateFilename)
		}
		// обновляем prev в памяти
		for k, v := range current {
			prev[k] = v
		}
		return
	}

	changes := compareGroupStates(prev, current)
	if len(changes) > 0 {
		for _, c := range changes {
			// syslog + mm
			if sysLogger != nil {
				_ = sysLogger.Warning(fmt.Sprintf("UserGroup change detected: %s", c))
			}
			if cfg.MattermostWebhook != "" {
				sendMattermostNotification(cfg, fmt.Sprintf("Изменения в UserGroup: %s", c), logger)
			}
			logger.Warnf("UserGroup change: %s", c)
		}
		// сохраняем новое состояние
		if err := saveGroupState(groupStateFilename, current, logger); err != nil {
			logger.Errorf("Ошибка сохранения состояния групп: %v", err)
		}
		// обновляем prev (в памяти)
		// пересоберём prev полностью на основе current
		for k := range prev {
			if _, ok := current[k]; !ok {
				delete(prev, k)
			}
		}
		for k, v := range current {
			prev[k] = v
		}
	}
}

// getUserGroups вызывает usergroup.get и собирает state
func getUserGroups(cfg *Config, logger *logrus.Logger) (GroupState, error) {
	req := ZabbixRequest{
		JSONRPC: "2.0",
		Method:  "usergroup.get",
		Params: map[string]interface{}{
			"output":      []string{"usrgrpid", "name"},
			"selectUsers": "extend",
		},
		Auth: cfg.APIToken,
		ID:   10,
	}
	jsonData, _ := json.Marshal(req)
	resp, err := http.Post(cfg.ZabbixAPIURL+"/api_jsonrpc.php", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var response struct {
		Result []struct {
			ID    string `json:"usrgrpid"`
			Name  string `json:"name"`
			Users []struct {
				UserID string `json:"userid"`
			} `json:"users"`
		} `json:"result"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Error.Code != 0 {
		return nil, fmt.Errorf("ошибка API (%d): %s - %s", response.Error.Code, response.Error.Message, response.Error.Data)
	}

	state := make(GroupState)
	for _, g := range response.Result {
		users := []string{}
		for _, u := range g.Users {
			users = append(users, u.UserID)
		}
		sort.Strings(users)
		state[g.ID] = UserGroup{ID: g.ID, Name: g.Name, Users: users}
	}
	logger.Infof("Получено %d пользовательских групп", len(state))
	return state, nil
}

func compareGroupStates(prev, curr GroupState) []string {
	changes := []string{}

	for id, cur := range curr {
		if p, ok := prev[id]; !ok {
			changes = append(changes, fmt.Sprintf("Добавлена группа: %s ", cur.Name))
		} else {

			if p.Name != cur.Name {
				changes = append(changes, fmt.Sprintf("Переименована группа %s -> %s ", p.Name, cur.Name))
			}

			if !stringSlicesEqual(p.Users, cur.Users) {

				changes = append(changes, fmt.Sprintf("Изменён состав пользователей в группе %s ", cur.Name))
			}
		}
	}

	for id, p := range prev {
		if _, ok := curr[id]; !ok {
			changes = append(changes, fmt.Sprintf("Удалена группа: %s ", p.Name))
		}
	}
	return changes
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
