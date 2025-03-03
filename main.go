package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/smtp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Config содержит настройки приложения.
type Config struct {
	ServerPort          int      `json:"serverPort"`          // например, 8080
	DBDriver            string   `json:"dbDriver"`            // например, "sqlite"
	DBSource            string   `json:"dbSource"`            // например, "gorm.db"
	SMTPHost            string   `json:"smtpHost"`            // SMTP сервер
	SMTPPort            int      `json:"smtpPort"`            // SMTP порт
	SMTPUsername        string   `json:"smtpUsername"`        // логин для SMTP
	SMTPPassword        string   `json:"smtpPassword"`        // пароль для SMTP
	AdminAllowedOrigins []string `json:"adminAllowedOrigins"` // список разрешённых доменов для CORS /admin
}

// Загружаем конфигурацию из файла config.json.
func loadConfig(filename string) (Config, error) {
	var cfg Config
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

// Модель Vacancy представляет вакансию.
type Vacancy struct {
	ID           uint           `gorm:"primaryKey" json:"id"`
	Title        string         `json:"title"`
	Subtitle     string         `json:"subtitle"`
	Description  string         `json:"description"`
	HeaderImage  string         `json:"headerImage"`
	BgGradient   string         `json:"bgGradient"`
	Requirements datatypes.JSON `json:"requirements"` // список требований в формате JSON
	TechStack    datatypes.JSON `json:"techStack"`    // список технологий в формате JSON
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// Модель Application представляет заявку.
type Application struct {
	ID                 uint           `gorm:"primaryKey" json:"id"`
	PrimaryContact     string         `json:"primaryContact"`
	AdditionalContacts datatypes.JSON `json:"additionalContacts"` // список контактов в формате JSON
	Name               string         `json:"name"`
	About              string         `json:"about"`
	VacancyID          uint           `json:"vacancyId"` // связь с вакансией
	Status             string         `json:"status"`    // например: "pending", "processed", "rejected"
	SalaryExpectation  string         `json:"salaryExpectation"`
	AvailableFrom      string         `json:"availableFrom"`
	CreatedAt          time.Time      `json:"createdAt"`
	UpdatedAt          time.Time      `json:"updatedAt"`
}

var (
	db  *gorm.DB
	cfg Config

	// Простое in-memory кэширование для вакансий.
	vacanciesCache      []Vacancy
	vacanciesCacheMutex sync.RWMutex
)

// invalidateVacanciesCache сбрасывает кэш вакансий.
func invalidateVacanciesCache() {
	vacanciesCacheMutex.Lock()
	vacanciesCache = nil
	vacanciesCacheMutex.Unlock()
}

// ----------------------
// ФУНКЦИИ ОТПРАВКИ EMAIL
// ----------------------

// sendEmail отправляет письмо через SMTP сервер согласно конфигурации.
func sendEmail(to, subject, body string) error {
	msg := "From: " + cfg.SMTPUsername + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\n" + body

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	auth := smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	return smtp.SendMail(addr, auth, cfg.SMTPUsername, []string{to}, []byte(msg))
}

// ----------------------
// PUBLIC API
// ----------------------

// getVacanciesHandler возвращает все вакансии, используя кэш, если он доступен.
func getVacanciesHandler(w http.ResponseWriter, r *http.Request) {
	vacanciesCacheMutex.RLock()
	if vacanciesCache != nil {
		cached := vacanciesCache
		vacanciesCacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"vacancies": cached})
		return
	}
	vacanciesCacheMutex.RUnlock()

	var vacancies []Vacancy
	if err := db.Find(&vacancies).Error; err != nil {
		http.Error(w, "Ошибка запроса вакансий", http.StatusInternalServerError)
		return
	}

	// Обновляем кэш
	vacanciesCacheMutex.Lock()
	vacanciesCache = vacancies
	vacanciesCacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"vacancies": vacancies})
}

// getVacancyHandler возвращает вакансию по ID, пытаясь сначала найти её в кэше.
func getVacancyHandler(w http.ResponseWriter, r *http.Request) {
	idStr := mux.Vars(r)["id"]
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Неверный формат ID", http.StatusBadRequest)
		return
	}

	vacanciesCacheMutex.RLock()
	if vacanciesCache != nil {
		for _, v := range vacanciesCache {
			if v.ID == uint(id) {
				vacanciesCacheMutex.RUnlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(v)
				return
			}
		}
	}
	vacanciesCacheMutex.RUnlock()

	var vacancy Vacancy
	if err := db.First(&vacancy, id).Error; err != nil {
		http.Error(w, "Вакансия не найдена", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vacancy)
}

// applyHandler сохраняет новую заявку в базу.
func applyHandler(w http.ResponseWriter, r *http.Request) {
	var app Application
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	app.Status = "pending" // начальный статус заявки
	if err := db.Create(&app).Error; err != nil {
		http.Error(w, "Ошибка сохранения заявки", http.StatusInternalServerError)
		return
	}
	log.Printf("Новая заявка: %+v\n", app)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "application received"})
}

// ----------------------
// ADMIN API - ВАКАНСИИ
// ----------------------

// addVacancyHandler добавляет новую вакансию.
func addVacancyHandler(w http.ResponseWriter, r *http.Request) {
	var v Vacancy
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := db.Create(&v).Error; err != nil {
		http.Error(w, "Ошибка создания вакансии", http.StatusInternalServerError)
		return
	}
	// Инвалидируем кэш
	invalidateVacanciesCache()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(v)
}

// updateVacancyHandler обновляет данные вакансии по ID.
func updateVacancyHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var v Vacancy
	if err := db.First(&v, id).Error; err != nil {
		http.Error(w, "Вакансия не найдена", http.StatusNotFound)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Гарантируем, что ID не изменится
	v.ID = 0 // не используем поле из JSON
	if err := db.Model(&Vacancy{}).Where("id = ?", id).Updates(v).Error; err != nil {
		http.Error(w, "Ошибка обновления вакансии", http.StatusInternalServerError)
		return
	}
	// Инвалидируем кэш
	invalidateVacanciesCache()

	var updated Vacancy
	db.First(&updated, id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

// deleteVacancyHandler удаляет вакансию по ID.
func deleteVacancyHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := db.Delete(&Vacancy{}, id).Error; err != nil {
		http.Error(w, "Ошибка удаления вакансии", http.StatusInternalServerError)
		return
	}
	// Инвалидируем кэш
	invalidateVacanciesCache()

	w.WriteHeader(http.StatusNoContent)
}

// getAdminVacanciesHandler возвращает все вакансии (админ-версия).
func getAdminVacanciesHandler(w http.ResponseWriter, r *http.Request) {
	getVacanciesHandler(w, r)
}

// ----------------------
// ADMIN API - ЗАЯВКИ
// ----------------------

// getApplicationsHandler возвращает все заявки.
func getApplicationsHandler(w http.ResponseWriter, r *http.Request) {
	var apps []Application
	if err := db.Find(&apps).Error; err != nil {
		http.Error(w, "Ошибка запроса заявок", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"applications": apps})
}

// getApplicationHandler возвращает заявку по ID.
func getApplicationHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var app Application
	if err := db.First(&app, id).Error; err != nil {
		http.Error(w, "Заявка не найдена", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// updateApplicationHandler обновляет заявку (например, меняет статус).
func updateApplicationHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var app Application
	if err := db.First(&app, id).Error; err != nil {
		http.Error(w, "Заявка не найдена", http.StatusNotFound)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := db.Save(&app).Error; err != nil {
		http.Error(w, "Ошибка обновления заявки", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// deleteApplicationHandler удаляет заявку по ID.
func deleteApplicationHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := db.Delete(&Application{}, id).Error; err != nil {
		http.Error(w, "Ошибка удаления заявки", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendEmailHandler отправляет письмо заявителю через SMTP.
func sendEmailHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var app Application
	if err := db.First(&app, id).Error; err != nil {
		http.Error(w, "Заявка не найдена", http.StatusNotFound)
		return
	}

	var payload struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Отправляем письмо на PrimaryContact (проверьте корректность email в реальной системе)
	if err := sendEmail(app.PrimaryContact, payload.Subject, payload.Body); err != nil {
		http.Error(w, "Ошибка отправки письма: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "email sent"})
}

// ----------------------
// MAIN
// ----------------------
func main() {
	// Загрузка конфигурации
	var err error
	cfg, err = loadConfig("config.json")
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	// Подключение к базе данных (в данном примере SQLite)
	db, err = gorm.Open(sqlite.Open(cfg.DBSource), &gorm.Config{})
	if err != nil {
		log.Fatalf("Ошибка подключения к БД: %v", err)
	}

	// Миграция моделей
	if err := db.AutoMigrate(&Vacancy{}, &Application{}); err != nil {
		log.Fatalf("Ошибка миграции: %v", err)
	}

	// Настройка роутера
	r := mux.NewRouter()

	// PUBLIC маршруты с глобальным CORS (допускаем все домены)
	public := r.PathPrefix("/api").Subrouter()
	public.HandleFunc("/vacancies", getVacanciesHandler).Methods("GET")
	public.HandleFunc("/vacancies/{id}", getVacancyHandler).Methods("GET")
	public.HandleFunc("/apply", applyHandler).Methods("POST")
	publicCors := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type"},
	})
	r.PathPrefix("/api").Handler(publicCors.Handler(public))

	// ADMIN маршруты (под префиксом /admin) – только с доменов, указанных в конфигурации.
	admin := mux.NewRouter()
	admin.HandleFunc("/vacancy", addVacancyHandler).Methods("POST")
	admin.HandleFunc("/vacancy/{id}", updateVacancyHandler).Methods("PUT")
	admin.HandleFunc("/vacancy/{id}", deleteVacancyHandler).Methods("DELETE")
	admin.HandleFunc("/vacancies", getAdminVacanciesHandler).Methods("GET")
	admin.HandleFunc("/applications", getApplicationsHandler).Methods("GET")
	admin.HandleFunc("/application/{id}", getApplicationHandler).Methods("GET")
	admin.HandleFunc("/application/{id}", updateApplicationHandler).Methods("PUT")
	admin.HandleFunc("/application/{id}", deleteApplicationHandler).Methods("DELETE")
	admin.HandleFunc("/application/{id}/send-email", sendEmailHandler).Methods("POST")

	adminCors := cors.New(cors.Options{
		AllowedOrigins:   cfg.AdminAllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type"},
		AllowCredentials: true,
	})
	// Оборачиваем admin маршруты в CORS-посредник.
	r.PathPrefix("/admin").Handler(adminCors.Handler(admin))

	// Определяем адрес сервера с использованием serverPort из конфигурации.
	addr := ":" + strconv.Itoa(cfg.ServerPort)
	srv := &http.Server{
		Handler:      r,
		Addr:         addr,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	log.Printf("Сервер запущен на %s\n", addr)
	log.Fatal(srv.ListenAndServe())
}
