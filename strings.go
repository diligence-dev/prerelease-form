package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
)

const defaultLang = "en"

var translations = map[string]map[string]string{
	"en": {
		"prerelease_signup":      "Prerelease Signup",
		"note_what_label":        "What",
		"note_when_label":        "When",
		"note_where_label":       "Where",
		"note_where_link":        "how to find us",
		"label_email":            "Email",
		"label_name":             "Name",
		"legend_format":          "Format",
		"seats_left":             "seats left",
		"waitlist_suffix":        " -> waitlist",
		"cancellation_ack_label": "I will email magicdraftberlin@posteo.de if I cannot come or am delayed",
		"data_consent_label":     "I consent to my data being processed for this event",
		"mailing_list_label":     "Optional: I want to join the mailing list for MtG (prerelease) events",
		"button_signup":          "Sign up",

		"title_pay":        "Pay for your spot",
		"heading_pay":      "Pay for your %s spot",
		"summary_wero":     "Pay via Wero",
		"scan_wero":        "Scan with Wero-compatible banking app:",
		"or_pay_via":       "Or pay via",
		"or_manually":      "Or manually:",
		"label_recipient":  "Recipient",
		"label_amount":     "Amount",
		"label_message":    "Message",
		"button_paid":      "Done, I've paid",
		"summary_iban":     "Pay via IBAN",
		"scan_banking":     "Scan with banking app:",
		"label_iban":       "IBAN",
		"label_bic":        "BIC",
		"label_reference":  "Reference",
		"summary_cash":     "Pay Cash at the event",
		"cash_info":        "You can pay in cash when you arrive at the event.",
		"button_cash":      "I'll pay cash at the event",
		"heading_thanks":   "Thanks!",
		"recorded_payment": "We've recorded your payment.",
		"recorded_cash":    "We've recorded that you'll pay cash at the event.",
		"look_forward":     "We look forward to seeing you at the event!",

		"title_waitlist":       "Waitlist",
		"heading_waitlist":     "You're on the waitlist",
		"waitlist_body_pre":    "We'll email you with payment details as soon as a seat opens up. If you no longer want the spot, ",
		"waitlist_cancel_link": "cancel",

		"title_cancel":          "Cancel Registration",
		"heading_cancel":        "Cancel Registration",
		"cancel_intro":          "Enter the email address you signed up with to cancel your registration or waitlist spot.",
		"button_confirm_cancel": "Confirm cancellation",

		"msg_server_error":           "server error",
		"msg_invalid_email":          "Invalid email.",
		"msg_invalid_name":           "Invalid name.",
		"msg_invalid_format":         "Invalid format.",
		"msg_invalid_form":           "invalid form",
		"msg_cancel_ack_required":    "Cancellation acknowledgement required.",
		"msg_data_consent_required":  "Data consent required.",
		"msg_already_registered":     "You have already registered with this email.",
		"msg_not_available":          "not available",
		"msg_no_registration":        "No registration found for that email.",
		"msg_already_cancelled":      "Your registration was already cancelled.",
		"msg_registration_cancelled": "Your registration has been cancelled.",
		"msg_waitlist_cancelled":     "Your waitlist spot has been cancelled.",
		"msg_invalid_method":         "Invalid method.",
		"msg_unknown_event":          "Event not found.",

		"subject_confirmed": "Prerelease - You're signed up",
		"subject_waitlist":  "Prerelease - You're on the waitlist",

		"body_confirmed": `Hi %s,

you are signed up for the prerelease - you will be playing %s!
If you haven't already, pay for your spot here: %s/%s/%s/pay?email=%s
If you can no longer attend, cancel at %s/%s/%s/cancel.
Looking forward to seeing you at the event!
`,
		"body_waitlist": `Hi %s,

Thanks for signing up for %s.
All %s seats are currently taken, so you've been added to the waitlist.
You'll receive another email with payment details as soon as a seat opens up for you.

If you no longer wish to be on the waitlist, cancel at %s/%s/%s/cancel.
`,
	},
	"de": {
		"prerelease_signup":      "Prerelease Anmeldung",
		"note_what_label":        "Was",
		"note_when_label":        "Wann",
		"note_where_label":       "Wo",
		"note_where_link":        "wie du uns findest",
		"label_email":            "E-Mail",
		"label_name":             "Name",
		"legend_format":          "Format",
		"seats_left":             "Plätze frei",
		"waitlist_suffix":        " -> Warteliste",
		"cancellation_ack_label": "Ich schreibe an magicdraftberlin@posteo.de, falls ich nicht kommen kann oder mich verspäte",
		"data_consent_label":     "Ich stimme der Verarbeitung meiner Daten für diese Veranstaltung zu",
		"mailing_list_label":     "Optional: Ich möchte dem Mailverteiler für MtG (Prerelease) Events beitreten",
		"button_signup":          "Anmelden",

		"title_pay":        "Bezahle deinen Platz",
		"heading_pay":      "Bezahle deinen %s-Platz",
		"summary_wero":     "Mit Wero bezahlen",
		"scan_wero":        "Scanne mit einer Wero-kompatiblen Banking-App:",
		"or_pay_via":       "Oder bezahle via",
		"or_manually":      "Oder manuell:",
		"label_recipient":  "Empfänger",
		"label_amount":     "Betrag",
		"label_message":    "Nachricht",
		"button_paid":      "Erledigt, ich habe bezahlt",
		"summary_iban":     "Mit IBAN bezahlen",
		"scan_banking":     "Scanne mit Banking-App:",
		"label_iban":       "IBAN",
		"label_bic":        "BIC",
		"label_reference":  "Verwendungszweck",
		"summary_cash":     "Bar vor Ort bezahlen",
		"cash_info":        "Du kannst bar bezahlen, wenn du vor Ort ankommst.",
		"button_cash":      "Ich zahle bar vor Ort",
		"heading_thanks":   "Danke!",
		"recorded_payment": "Wir haben deine Zahlung erfasst.",
		"recorded_cash":    "Wir haben erfasst, dass du bar vor Ort zahlen wirst.",
		"look_forward":     "Wir freuen uns, dich bei der Veranstaltung zu sehen!",

		"title_waitlist":       "Warteliste",
		"heading_waitlist":     "Du bist auf der Warteliste",
		"waitlist_body_pre":    "Wir werden dir per E-Mail Zahlungsdaten schicken, sobald ein Platz frei wird. Wenn du den Platz nicht mehr möchtest, ",
		"waitlist_cancel_link": "storniere",

		"title_cancel":          "Anmeldung stornieren",
		"heading_cancel":        "Anmeldung stornieren",
		"cancel_intro":          "Gib die E-Mail-Adresse ein, mit der du dich angemeldet hast, um deine Anmeldung oder deinen Wartelistenplatz zu stornieren.",
		"button_confirm_cancel": "Stornierung bestätigen",

		"msg_server_error":           "Serverfehler",
		"msg_invalid_email":          "Ungültige E-Mail.",
		"msg_invalid_name":           "Ungültiger Name.",
		"msg_invalid_format":         "Ungültiges Format.",
		"msg_invalid_form":           "ungültiges Formular",
		"msg_cancel_ack_required":    "Stornierungsbestätigung erforderlich.",
		"msg_data_consent_required":  "Dateneinwilligung erforderlich.",
		"msg_already_registered":     "Du hast dich bereits mit dieser E-Mail angemeldet.",
		"msg_not_available":          "nicht verfügbar",
		"msg_no_registration":        "Für diese E-Mail wurde keine Anmeldung gefunden.",
		"msg_already_cancelled":      "Deine Anmeldung wurde bereits storniert.",
		"msg_registration_cancelled": "Deine Anmeldung wurde storniert.",
		"msg_waitlist_cancelled":     "Dein Wartelistenplatz wurde storniert.",
		"msg_invalid_method":         "Ungültige Methode.",
		"msg_unknown_event":          "Veranstaltung nicht gefunden.",

		"subject_confirmed": "Prerelease - Du bist angemeldet",
		"subject_waitlist":  "Prerelease - Du bist auf der Warteliste",

		"body_confirmed": `Hallo %s,

du bist für den Prerelease angemeldet - du wirst %s spielen!
Falls noch nicht geschehen, bezahle hier: %s/%s/%s/pay?email=%s
Wenn du nicht mehr teilnehmen kannst, storniere unter %s/%s/%s/cancel.
Wir freuen uns, dich bei der Veranstaltung zu sehen!
`,
		"body_waitlist": `Hallo %s,

danke für deine Anmeldung für %s.
Alle %s Plätze sind derzeit belegt, also wurdest du zur Warteliste hinzugefügt.
Du wirst eine weitere E-Mail mit Zahlungsdaten erhalten, sobald ein Platz für dich frei wird.

Wenn du nicht mehr auf der Warteliste sein möchtest, storniere unter %s/%s/%s/cancel.
`,
	},
}

func T(lang, key string, args ...any) string {
	langMap, ok := translations[lang]
	if !ok {
		langMap = translations[defaultLang]
	}
	msg, ok := langMap[key]
	if !ok {
		log.Printf("missing translation: lang=%s key=%s", lang, key)
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

func parseAcceptLanguage(header string) string {
	bestLang := defaultLang
	bestQ := -1.0
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, qStr, _ := strings.Cut(part, ";")
		tag = strings.TrimSpace(tag)
		q := 1.0
		if strings.HasPrefix(qStr, "q=") {
			if v, err := strconv.ParseFloat(strings.TrimPrefix(qStr, "q="), 64); err == nil {
				q = v
			}
		}
		primary := strings.ToLower(tag)
		if i := strings.Index(primary, "-"); i >= 0 {
			primary = primary[:i]
		}
		if (primary == "en" || primary == "de") && q > 0 && q > bestQ {
			bestLang = primary
			bestQ = q
		}
	}
	return bestLang
}
