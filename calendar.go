package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

type CalendarService struct {
	srv   *calendar.Service
	calID string
}

type TenantCalendarConfig struct {
	CalendarID string `json:"calendar_id"`
}

func NewCalendarService(tenant string) (*CalendarService, error) {
	ctx := context.Background()
	credsFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credsFile == "" {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS no está en .env")
	}

	configRoot := "configs"
	configPath := filepath.Join(configRoot, tenant, "calendar.json")

	calID := ""
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		calID = os.Getenv("GOOGLE_CALENDAR_ID")
	} else {
		b, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("error leyendo config calendario tenant: %w", err)
		}
		var cfg TenantCalendarConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("json calendario inválido: %w", err)
		}
		calID = cfg.CalendarID
	}

	if calID == "" {
		return nil, fmt.Errorf("no se encontró calendar_id para el tenant %s", tenant)
	}

	srv, err := calendar.NewService(ctx, option.WithCredentialsFile(credsFile))
	if err != nil {
		return nil, fmt.Errorf("error creando cliente calendar: %v", err)
	}

	return &CalendarService{
		srv:   srv,
		calID: calID,
	}, nil
}

type Slot struct {
	ID       string
	Text     string
	ISOValue string
}

// GetNextAvailableSlots ahora usa la zona horaria de Buenos Aires
func (c *CalendarService) GetNextAvailableSlots() ([]Slot, error) {
	// 1. Cargamos la zona horaria
	loc, err := time.LoadLocation("America/Argentina/Buenos_Aires")
	if err != nil {
		// Fallback por si no encuentra la zona (ej: windows sin tzdata)
		fmt.Printf("⚠️ No se pudo cargar zona horaria, usando Local: %v\n", err)
		loc = time.Local
	}

	// 2. Usamos 'now' en ESA zona
	now := time.Now().In(loc)

	minTime := now.Format(time.RFC3339)
	maxTime := now.Add(72 * time.Hour).Format(time.RFC3339)

	query := &calendar.FreeBusyRequest{
		TimeMin: minTime,
		TimeMax: maxTime,
		Items:   []*calendar.FreeBusyRequestItem{{Id: c.calID}},
	}

	res, err := c.srv.Freebusy.Query(query).Do()
	if err != nil {
		return nil, err
	}

	busyRanges := res.Calendars[c.calID].Busy
	var slots []Slot

	counter := 1
	for d := 0; d < 3; d++ {
		day := now.AddDate(0, 0, d)

		// 3. Iteramos las horas. OJO: Esto es de 09 a 17 hora ARGENTINA
		for h := 9; h < 17; h++ {
			// Creamos la fecha usanda la location 'loc' (Buenos Aires)
			slotStart := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
			slotEnd := slotStart.Add(1 * time.Hour)

			if slotStart.Before(now) {
				continue
			}

			isBusy := false
			for _, busy := range busyRanges {
				bStart, _ := time.Parse(time.RFC3339, busy.Start)
				bEnd, _ := time.Parse(time.RFC3339, busy.End)

				// Comparamos peras con peras (time.Time maneja las zonas internamente)
				if slotStart.Before(bEnd) && slotEnd.After(bStart) {
					isBusy = true
					break
				}
			}

			if !isBusy {
				slots = append(slots, Slot{
					ID:   fmt.Sprintf("SLOT_%d", counter),
					Text: fmt.Sprintf("%s %s", slotStart.Format("Mon 02"), slotStart.Format("15:04")),
					// El ISO ahora llevará el offset correcto (-03:00)
					ISOValue: slotStart.Format(time.RFC3339),
				})
				counter++
				if len(slots) >= 3 {
					return slots, nil
				}
			}
		}
	}
	return slots, nil
}

func (c *CalendarService) CreateAppointment(isoStart, contactName, contactPhone string) error {
	// Parseamos respetando el offset que viene en el string (ej: -03:00)
	startTime, err := time.Parse(time.RFC3339, isoStart)
	if err != nil {
		return fmt.Errorf("fecha inválida: %v", err)
	}
	endTime := startTime.Add(1 * time.Hour)

	summary := fmt.Sprintf("Turno Flowly: %s", contactName)
	desc := fmt.Sprintf("Paciente agendado vía WhatsApp.\nTeléfono: %s", contactPhone)

	event := &calendar.Event{
		Summary:     summary,
		Description: desc,
		Start: &calendar.EventDateTime{
			DateTime: startTime.Format(time.RFC3339),
		},
		End: &calendar.EventDateTime{
			DateTime: endTime.Format(time.RFC3339),
		},
	}

	_, err = c.srv.Events.Insert(c.calID, event).Do()
	return err
}
