package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		er := http.StatusMethodNotAllowed
		http.Error(w, "Invalid method", er)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	if len(username) < 1 || len(password) < 8 {
		er := http.StatusNotAcceptable
		http.Error(w, "Invalid username/password", er)
		return
	}

	// Check if the user already exists
	_, err := GetUserByUsername(username)
	if err == nil {
		http.Error(w, "User already exists", http.StatusConflict)
		return
	}

	// Hash the password
	hashedPassword, err := hashPassword(password)
	if err != nil {
		http.Error(w, "Error processing password", http.StatusInternalServerError)
		return
	}

	// Save the user to the database
	err = SaveUser(username, hashedPassword)
	if err != nil {
		http.Error(w, "Error saving user", http.StatusInternalServerError)
		return
	}

	log.Println("User registered successfully")
	w.WriteHeader(http.StatusCreated)
}

func LoginUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("LoginUser error: invalid request method %s", r.Method)
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == "" || password == "" {
		log.Printf("LoginUser error: missing username or password. Username: %s", username)
		http.Error(w, "Missing username or password", http.StatusBadRequest)
		return
	}

	// Fetch user from database
	user, err := GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Invalid username or password", http.StatusUnauthorized)
			log.Printf("Login failed: User not found with username '%s'", username)
		} else {
			http.Error(w, "Error retrieving user", http.StatusInternalServerError)
			log.Printf("Error retrieving user from database: %v", err)
		}
		return
	}

	// Validate password
	if !checkPasswordHash(password, user.HashedPassword) {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		log.Printf("Login failed: Invalid password for username '%s'", username)
		return
	}

	// Generate session and CSRF tokens
	sessionToken := generateToken(32)
	csrfToken := generateToken(32)

	// Sets the session cookies.
	// This will be automatically sent by the browser for any requests to our endpoints on the same domain.
	// Hence this introduces CSRF vulnerabilities because the cookie will automatically be sent allowing forged cross-origin requests.
	// HttpOnly and Secure flags mitigate risks like XSS and data interception.
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,                    // Ensures the session token cant be accessed by front-end JavaScript and only sent during HTTP requests. Reducing XSS risk.
		Secure:   true,                    // Ensures that the cookie is only sent over HTTPS connections, preventing interception over insecure HTTP. If Secure is not set explicitly, the cookie will be sent over both HTTP and HTTPS.
		SameSite: http.SameSiteStrictMode, // Controls whether cookies are sent with cross-site requests, mitigating CSRF risks. The default for SameSite is unset, which allows cookies to be sent with cross-origin requests.
	})

	// Sets the CSRF Token
	// When the CSRF token is sent back to the server for authentication, the user must explisitly send it in a custom request header.
	// Because the custom request header (tippicaly called "X-CSRF-Token") is added by the client and not sent automaticaly, Same-Origin
	// Policy stops malicious websites from accessing this and only we are able to get and attach the csrf-token to the x-csrf-token request header.
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    csrfToken,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: false, // Needs to be accessable client side to be added to request headers
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})

	// Update the user's session and CSRF tokens in the database
	err = UpdateSessionAndCSRF(user.ID, sessionToken, csrfToken)
	if err != nil {
		http.Error(w, "Error updating session", http.StatusInternalServerError)
		log.Printf("Error updating session: %v", err)
		return
	}

	log.Println("Login Successfull")
	w.WriteHeader(http.StatusOK)
}

func LogoutUser(w http.ResponseWriter, r *http.Request) {
	user, err := authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Clear Token Cookies
	setCookie(w, "session_token", "", true, true)
	setCookie(w, "csrf_token", "", false, true)

	// Clear session and CSRF tokens in the database
	err = ClearSession(user.ID)
	if err != nil {
		http.Error(w, "Error clearing session", http.StatusInternalServerError)
		return
	}

	fmt.Fprintln(w, "Logged out.")
}

func Profile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	user, err := authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		log.Printf("Error authorizing session: %v", err)
		return
	}

	fmt.Fprintf(w, "Authorized, welcome %s", user.Username)
}

func hashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 10) // Cost = 10 means the password is hashed 2^10 times.
	// This is to slow down any attempt to "hash crack", ie, reverse engineer the password by making guesses and seeing if that matches the hashed password
	// Note: bcrypt also automaticaly handles salting to protect against prcomputed hash table attacks.

	return string(bytes), err
}

func checkPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

func generateToken(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		log.Fatalf("Failed to generate token: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(bytes)
}

func authorize(r *http.Request) (*User, error) {
	sessionToken, err := r.Cookie("session_token")
	if err != nil || sessionToken.Value == "" {
		log.Printf("Authorization failed: Missing or empty session token. Error: %v", err)
		return nil, errors.New("missing session token")
	}

	csrfToken := r.Header.Get("X-CSRF-Token")
	if csrfToken == "" {
		log.Println("Authorization failed: Missing CSRF token in request header.")
		return nil, errors.New("missing CSRF token")
	}

	user, err := GetUserBySessionToken(sessionToken.Value)
	if err != nil {
		log.Printf("Authorization failed: Unable to fetch user for session token %s. Error: %v", sessionToken.Value, err)
		return nil, errors.New("unauthorized")
	}

	if user.CSRFToken != csrfToken {
		log.Printf("Authorization failed: CSRF token mismatch for user %s. Expected: %s, Received: %s",
			user.Username, user.CSRFToken, csrfToken)
		return nil, errors.New("unauthorized")
	}

	log.Printf("Authorization successful for user: %s", user.Username)
	return &user, nil
}

func setCookie(w http.ResponseWriter, name, value string, httpOnly, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}