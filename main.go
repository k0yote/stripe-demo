package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/joho/godotenv"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/paymentmethod"
	"github.com/stripe/stripe-go/v81/setupintent"
	"github.com/stripe/stripe-go/v81/webhook"
)

// Replace with your actual Stripe keys:
var (
	// Secret key from https://dashboard.stripe.com/apikeys
	stripeSecretKey string
	// Webhook signing secret from https://dashboard.stripe.com/webhooks
	stripeWebhookSecret string
)

// In a real app, you'd store these in a database:
var (
	// Map: email => Stripe customer ID
	userCustomerMap = make(map[string]string)

	// Map: email => PaymentMethod IDs (one user could have multiple)
	userPaymentMethodsMap = make(map[string][]string)

	// paymentProcessMap: paymentID -> PaymentStatus
	paymentProcessMap = make(map[string]string)

	// A simple mutex to avoid concurrent map access
	mu sync.Mutex
)

func main() {
	// get secret key from .env file

	// Initialize Stripe
	stripe.Key = stripeSecretKey

	// Serve static files (frontend) from the "static" folder
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	// config public key
	http.HandleFunc("/api/config", handleConfig)

	// Create SetupIntent endpoint
	http.HandleFunc("/api/setup-intent", createSetupIntentHandler)

	// --------------------
	// User Card Information
	// --------------------
	http.HandleFunc("/api/user-cards", getUserCardsHandler)    // GET  (retrieve user card info)
	http.HandleFunc("/api/update-card", updateUserCardHandler) // POST (create SetupIntent => save new card)
	http.HandleFunc("/api/remove-card", removeUserCardHandler) // POST detach a card

	// Payment flow: Start -> Check -> Finish
	http.HandleFunc("/api/start-payment", startPaymentHandler)   // POST
	http.HandleFunc("/api/check-payment", checkPaymentHandler)   // GET
	http.HandleFunc("/api/finish-payment", finishPaymentHandler) // POST
	http.HandleFunc("/api/cancel-payment", cancelPaymentHandler) // POST cancel PaymentIntent

	// Webhook endpoint
	http.HandleFunc("/webhook", webhookHandler)

	fmt.Println("Server running on http://localhost:9080")
	log.Fatal(http.ListenAndServe(":9080", nil))
	//stripe listen --forward-to localhost:8080/webhook
	//stripe trigger payment_intent.succeeded
}

// createSetupIntentHandler creates a SetupIntent and returns its client_secret.
func createSetupIntentHandler(w http.ResponseWriter, r *http.Request) {
	// For simplicity, use r.FormValue here. In production, parse JSON properly.
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	if email == "" {
		http.Error(w, "Missing email", http.StatusBadRequest)
		return
	}

	// 1. Create or retrieve a Stripe customer for this user
	customerID, ok := userCustomerMap[email]
	var c *stripe.Customer
	if !ok {
		// Create a new Stripe customer
		cParams := &stripe.CustomerParams{Metadata: map[string]string{"community_id": "1"}}
		c, err = customer.New(cParams)
		if err != nil {
			http.Error(w, "Failed to create customer", http.StatusInternalServerError)
			return
		}
		customerID = c.ID
		userCustomerMap[email] = c.ID
	}

	_ = customerID

	customer, err := customer.Get("cus_RUC2kbS3mP6ab2", nil)
	if err != nil {
		http.Error(w, "Failed to get customer", http.StatusInternalServerError)
		return
	}
	fmt.Printf("1---------------------stripe.Customer: %+v", customer)

	// 2. Create a SetupIntent for the customer
	params := &stripe.SetupIntentParams{
		// Customer:           stripe.String(customer.ID),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
	}
	si, err := setupintent.New(params)
	if err != nil {
		http.Error(w, "Failed to create SetupIntent", http.StatusInternalServerError)
		return
	}

	fmt.Printf("1---------------------stripe.Customer: %+v", c)
	fmt.Printf("1---------------------stripe.SetupIntent: %+v", si)

	// Return the client_secret so the frontend can confirm the card setup
	w.Write([]byte(si.ClientSecret))
}

// webhookHandler handles Stripe webhook events like setup_intent.succeeded
func webhookHandler(w http.ResponseWriter, r *http.Request) {
	const MaxBodyBytes = int64(65536)
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Verify Stripe webhook signature
	sig := r.Header.Get("Stripe-Signature")
	event, err := webhook.ConstructEvent(payload, sig, stripeWebhookSecret)
	if err != nil {
		fmt.Printf("⚠️  Webhook signature verification failed: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Process the event
	switch event.Type {
	case "setup_intent.created":
		var si stripe.SetupIntent
		if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
			fmt.Printf("Error parsing setup_intent.succeeded event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// The SetupIntent is complete, and a PaymentMethod is saved.
		fmt.Printf("✅ setup_intent.created: %s\n", si.ID)
		fmt.Printf("stripe.SetupIntent: %+v\n", si)

		// Optionally store the PaymentMethod for the user in memory (or DB)
		// We need to look up which email this customer belongs to. For a real app, store
		// the relationship between customer ID and user email in the DB.
		// email := findEmailByCustomerID(si.Customer.ID)
		// if email != "" {
		// 	userPaymentMethodsMap[email] = append(userPaymentMethodsMap[email], si.PaymentMethod.ID)
		// 	fmt.Printf("Stored PaymentMethod %s for user %s\n", si.PaymentMethod.ID, email)
		// }

	case "setup_intent.setup_failed":
		var si stripe.SetupIntent
		if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
			fmt.Printf("Error parsing setup_intent.setup_failed event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("❌ SetupIntent failed: %s\n", si.LastSetupError.Msg)
	case "payment_method.attached":
		var sp stripe.PaymentMethod
		if err := json.Unmarshal(event.Data.Raw, &sp); err != nil {
			fmt.Printf("Error parsing payment_method.attached event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ setup_intent.created: %s\n", sp.ID)
		fmt.Printf("payment_method.attached: %+v\n", sp)
	case "setup_intent.succeeded":
		var si stripe.SetupIntent
		if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
			fmt.Printf("Error parsing setup_intent.succeeded event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// The SetupIntent is complete, and a PaymentMethod is saved.
		fmt.Printf("✅ setup_intent.succeeded: %s\n", si.ID)
		fmt.Printf("stripe.SetupIntent: %+v\n", si)
	case "customer.created":
		var cc stripe.Customer
		if err := json.Unmarshal(event.Data.Raw, &cc); err != nil {
			fmt.Printf("Error parsing customer.created event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ customer.created: %s\n", cc.ID)
		fmt.Printf("stripe.Customer: %+v\n", cc)
	case "payment_intent.created":
		var pi stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			fmt.Printf("Error parsing customer.created event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ customer.created: %s\n", pi.ID)
		fmt.Printf("stripe.Customer: %+v\n", pi)
	case "payment_intent.amount_capturable_updated":
		var pi stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			fmt.Printf("Error parsing payment_intent.amount_capturable_updated event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ payment_intent.amount_capturable_updated: %s\n", pi.ID)
		fmt.Printf("stripe.PaymentIntent: %+v\n", pi)
	case "charge.succeeded":
		var cs stripe.Charge
		if err := json.Unmarshal(event.Data.Raw, &cs); err != nil {
			fmt.Printf("Error parsing charge.succeeded event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ charge.succeeded: %s\n", cs.ID)
		fmt.Printf("stripe.Charge: %+v\n", cs)
	case "payment_intent.canceled":
		var pi stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			fmt.Printf("Error parsing payment_intent.canceled event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ payment_intent.canceled: %s\n", pi.ID)
		fmt.Printf("stripe.PaymentIntent: %+v\n", pi)
	case "refund.created":
		var r stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &r); err != nil {
			fmt.Printf("Error parsing refund.created event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ refund.created: %s\n", r.ID)
		fmt.Printf("stripe.Refund: %+v\n", r)
	case "charge.refunded":
		var sc stripe.Charge
		if err := json.Unmarshal(event.Data.Raw, &sc); err != nil {
			fmt.Printf("Error parsing charge.refunded event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ charge.refunded: %s\n", sc.ID)
		fmt.Printf("stripe.Charge: %+v\n", sc)
	case "refund.updated":
		var r stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &r); err != nil {
			fmt.Printf("Error parsing refund.updated event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ refund.updated: %s\n", r.ID)
		fmt.Printf("stripe.Refund: %+v\n", r)
	case "charge.refund.updated":
		var r stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &r); err != nil {
			fmt.Printf("Error parsing charge.refund.updated event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ charge.refund.updated: %s\n", r.ID)
		fmt.Printf("stripe.Refund: %+v\n", r)
	case "payment_intent.succeeded":
		var pi stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			fmt.Printf("Error parsing payment_intent.succeeded event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ payment_intent.succeeded: %s\n", pi.ID)
		fmt.Printf("stripe.PaymentIntent: %+v\n", pi)
	case "charge.captured":
		var c stripe.Charge
		if err := json.Unmarshal(event.Data.Raw, &c); err != nil {
			fmt.Printf("Error parsing charge.captured event: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Printf("✅ charge.captured: %s\n", c.ID)
		fmt.Printf("stripe.Charge: %+v\n", c)
	default:
		// Other event types ...
		fmt.Printf("Unhandled event type: %s\n", event.Type)
	}

	w.WriteHeader(http.StatusOK)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"publishableKey": os.Getenv("STRIPE_PUBLISHABLE_KEY"),
	})
}

// --------------------
// 1. USER CARD API
// --------------------

// GET /api/user-cards?email=...
// Returns the saved PaymentMethods for a given user (by email).
func getUserCardsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		email := r.URL.Query().Get("email")
		if email == "" {
			http.Error(w, "Missing email", http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods := userPaymentMethodsMap[email]
		mu.Unlock()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"email":           email,
			"payment_methods": methods,
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// POST /api/update-card
// Creates a SetupIntent to save a new card for the user.
// Expects: email (string)
func updateUserCardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// For simplicity, parse form
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	if email == "" {
		http.Error(w, "Missing email", http.StatusBadRequest)
		return
	}

	// Create or retrieve existing customer
	mu.Lock()
	customerID, exists := userCustomerMap[email]
	mu.Unlock()
	if !exists {
		// Create new customer in Stripe
		cparams := &stripe.CustomerParams{Email: stripe.String(email)}
		c, err := customer.New(cparams)
		if err != nil {
			http.Error(w, "Failed to create Stripe customer", http.StatusInternalServerError)
			return
		}
		customerID = c.ID
		mu.Lock()
		userCustomerMap[email] = c.ID
		mu.Unlock()
	}

	// Create SetupIntent
	siParams := &stripe.SetupIntentParams{
		Customer:           stripe.String(customerID),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
	}
	si, err := setupintent.New(siParams)
	if err != nil {
		http.Error(w, "Failed to create SetupIntent", http.StatusInternalServerError)
		return
	}

	// Return client_secret so frontend can confirm card setup
	// (The user will use stripe.confirmCardSetup(...) in the frontend)
	w.Write([]byte(si.ClientSecret))
}

// POST /api/remove-card
// Detach a PaymentMethod from the customer's account
// Expects: form fields: email, payment_method_id
func removeUserCardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	email := r.FormValue("email")
	pmID := r.FormValue("payment_method_id")

	if email == "" || pmID == "" {
		http.Error(w, "Missing email or payment_method_id", http.StatusBadRequest)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// Detach the PaymentMethod from the Stripe customer
	// This does NOT delete the PaymentMethod object entirely, but removes it from the customer
	_, err = paymentmethod.Detach(pmID, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to detach PaymentMethod: %v", err), http.StatusInternalServerError)
		return
	}

	// Remove the pmID from our local map
	if methods, ok := userPaymentMethodsMap[email]; ok {
		newList := []string{}
		for _, m := range methods {
			if m != pmID {
				newList = append(newList, m)
			}
		}
		userPaymentMethodsMap[email] = newList
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "payment method detached",
		"pm_id":  pmID,
	})
}

// --------------------
// 2. PAYMENT FLOW
// --------------------

// The typical payment flow might be:
//   1) /api/start-payment -> Create PaymentIntent (unconfirmed or ephemeral charge data)
//   2) /api/check-payment -> Poll or check status, or do other logic (not always needed, but you requested it)
//   3) /api/finish-payment -> Confirm/finalize PaymentIntent (after verifying external services, etc.)

// POST /api/start-payment
// Body: email, amount (in cents), payment_method_id
// Creates a PaymentIntent (but let's say we don't confirm it yet).
func startPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	amountStr := r.FormValue("amount") // e.g., "2000" for $20.00
	paymentMethodID := r.FormValue("payment_method_id")

	if email == "" || amountStr == "" || paymentMethodID == "" {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		return
	}

	// Convert amount from string to int64
	var amount int64
	_, err = fmt.Sscanf(amountStr, "%d", &amount)
	if err != nil {
		http.Error(w, "Invalid amount format", http.StatusBadRequest)
		return
	}

	// Retrieve the Stripe customer ID
	mu.Lock()
	customerID, exists := userCustomerMap[email]
	mu.Unlock()
	if !exists {
		http.Error(w, "User not found in system", http.StatusBadRequest)
		return
	}

	// Create PaymentIntent in Stripe (unconfirmed for now)
	piParams := &stripe.PaymentIntentParams{
		Amount:        stripe.Int64(amount),
		Currency:      stripe.String("usd"),
		Customer:      stripe.String(customerID),
		PaymentMethod: stripe.String(paymentMethodID),
		// If you want to confirm right away, set Confirm=true.
		// If you want to do a 2-step flow, keep it false, or create ephemeral secret for the client.
		// For demonstration, let's do a 3-step flow on the backend.
		ConfirmationMethod: stripe.String(string(stripe.PaymentIntentConfirmationMethodManual)),
		CaptureMethod:      stripe.String(string(stripe.PaymentIntentCaptureMethodManual)),
	}
	pi, err := paymentintent.New(piParams)
	if err != nil {
		http.Error(w, "Failed to create PaymentIntent", http.StatusInternalServerError)
		return
	}

	// Store the PaymentIntent status in memory for demonstration
	mu.Lock()
	paymentProcessMap[pi.ID] = "started"
	mu.Unlock()

	// Return PaymentIntent ID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"payment_intent_id": pi.ID,
		"status":            string(pi.Status),
	})
}

// GET /api/check-payment?payment_intent_id=...
// Check the status of the PaymentIntent (or do other business logic).
func checkPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	paymentIntentID := r.URL.Query().Get("payment_intent_id")
	if paymentIntentID == "" {
		http.Error(w, "Missing payment_intent_id", http.StatusBadRequest)
		return
	}

	// Retrieve PaymentIntent from Stripe (optional, or check from local map)
	pi, err := paymentintent.Get(paymentIntentID, nil)
	if err != nil {
		http.Error(w, "Failed to retrieve PaymentIntent", http.StatusInternalServerError)
		return
	}

	// Also retrieve local status if needed
	mu.Lock()
	localStatus := paymentProcessMap[paymentIntentID]
	mu.Unlock()

	// Return combined status info
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"payment_intent_id": pi.ID,
		"stripe_status":     pi.Status,
		"local_status":      localStatus,
	})
}

// POST /api/finish-payment
// Body: payment_intent_id
// Suppose you've checked external service -> everything is OK -> Now finalize the payment
func finishPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	paymentIntentID := r.FormValue("payment_intent_id")
	if paymentIntentID == "" {
		http.Error(w, "Missing payment_intent_id", http.StatusBadRequest)
		return
	}

	// Let's confirm & capture the PaymentIntent
	// (If you used manual confirmation, you might need to confirm first, then capture)
	// This depends on how your PaymentIntent is configured.
	confirmParams := &stripe.PaymentIntentConfirmParams{}
	pi, err := paymentintent.Confirm(paymentIntentID, confirmParams)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to confirm PaymentIntent: %v", err), http.StatusInternalServerError)
		return
	}

	// Now capture if using manual capture
	if pi.CaptureMethod == stripe.PaymentIntentCaptureMethodManual && pi.Status == stripe.PaymentIntentStatusRequiresCapture {
		pi, err = paymentintent.Capture(paymentIntentID, nil)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to capture PaymentIntent: %v", err), http.StatusInternalServerError)
			return
		}
	}

	mu.Lock()
	paymentProcessMap[paymentIntentID] = "finished"
	mu.Unlock()

	// Return updated PaymentIntent
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"payment_intent_id": pi.ID,
		"final_status":      pi.Status,
	})
}

// POST /api/cancel-payment
// Body: payment_intent_id
// Cancel an in-progress PaymentIntent
func cancelPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	paymentIntentID := r.FormValue("payment_intent_id")
	if paymentIntentID == "" {
		http.Error(w, "Missing payment_intent_id", http.StatusBadRequest)
		return
	}

	pi, err := paymentintent.Cancel(paymentIntentID, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to cancel PaymentIntent: %v", err), http.StatusInternalServerError)
		return
	}

	mu.Lock()
	paymentProcessMap[paymentIntentID] = "canceled"
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"payment_intent_id": pi.ID,
		"status":            string(pi.Status),
	})
}

// findEmailByCustomerID is a helper to reverse-lookup an email from our map
func findEmailByCustomerID(customerID string) string {
	for email, cID := range userCustomerMap {
		if cID == customerID {
			return email
		}
	}
	return ""
}

func init() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	stripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	if stripeSecretKey == "" {
		log.Fatal("Missing STRIPE_SECRET_KEY in .env file")
	}

	stripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	if stripeWebhookSecret == "" {
		log.Fatal("Missing STRIPE_WEBHOOK_SECRET in .env file")
	}
}
