package main

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"database/sql"
	"encoding/json"

	"github.com/edgelesssys/ego/ecrypto"
	"github.com/edgelesssys/ego/enclave"
	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/square/go-jose.v2/jwt"
)

// serverAddr is the address of the server
const serverAddr = "0.0.0.0:8080"
const saltSize = 16
const maxAttempts = 3

var token string
var err error

// attestationProviderURL is the URL of the attestation provider
const attestationProviderURL = "https://shareduks.uks.attest.azure.net"

func main() {
	// Create a self signed certificate.
	cert, priv := createCertificate()
	fmt.Println("🆗 Generated Certificate.")

	// Cerate an Azure Attestation Token.
	token, err = enclave.CreateAzureAttestationToken(cert, attestationProviderURL)
	if err != nil {
		panic(err)
	}

	ctx, _ := context.WithCancel(context.Background())
	go checkTokenExpiration(ctx, token, cert)

	fmt.Println("🆗 Created an Microsoft Azure Attestation Token.")

	//create database
	database, err := sql.Open("sqlite3", "./data/password.db")
	if err != nil {
		panic(err)
	}
	//create Table for username,Hmac and salt.
	statement, _ := database.Prepare("CREATE TABLE IF NOT EXISTS Hmac (username varchar(50) PRIMARY KEY, hmac varchar(128), salt BLOB)")
	statement.Exec()

	//create Table for Hmac key
	statement, _ = database.Prepare("CREATE TABLE IF NOT EXISTS Sealed (Hmackey BLOB PRIMARY KEY)")
	statement.Exec()

	//create Table for salt_with_attempt
	statement, _ = database.Prepare("CREATE TABLE IF NOT EXISTS salt_with_attempt (data BLOB PRIMARY KEY)")
	statement.Exec()

	//create Table for attemp resetTime
	statement, _ = database.Prepare("CREATE TABLE IF NOT EXISTS resetTime (time BLOB PRIMARY KEY)")
	statement.Exec()

	//create Table for Token key
	statement, _ = database.Prepare("CREATE TABLE IF NOT EXISTS Token (username varchar(50) PRIMARY KEY, Token varchar(256))")
	statement.Exec()

	//generate a random hmac key
	hmacKey, salt_with_attempt, resetTime, err := initialize(database)
	if err != nil {
		fmt.Println(err)
	}

	// Create HTTPS server.
	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(token)) })
	http.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("📫 %v sent secret %v\n", r.RemoteAddr, r.URL.Query()["s"])
	})

	//register
	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Only GET requests are allowed", http.StatusBadRequest)
			return
		}

		err := r.ParseForm()
		if err != nil {
			http.Error(w, "Failed to parse request body", http.StatusBadRequest)
			return
		}

		username := r.FormValue("username")
		pwd := r.FormValue("password")

		fmt.Printf("📫 %v sent username %v\n", r.RemoteAddr, username)
		fmt.Printf("📫 %v sent password %v\n", r.RemoteAddr, pwd)

		// generate a random salt with 10 rounds of complexity
		var salt = generateRandomSalt(saltSize)
		saltKey := fmt.Sprintf("%x", salt)

		//salting the password
		var salting = salting(pwd, salt)

		//generate hmac
		var hmac = genHmac(salting, hmacKey)

		//insert data into DB
		if err := AddSaltAndHmac(username, hmac, salt, database); err != nil {

			//if the username already exist in DB sent err
			//w.Write([]byte(fmt.Sprintf("username %s already exist in DB. Error: %s", username, err.Error())))
			w.Write([]byte(`<!DOCTYPE html>
			<html>
			  <head>
				<meta charset='utf-8'>
				<title>registeration failed</title>
			  </head>
			  <body>
				<h1>Sorry, your registration is failed.</h1>
				<h1>the username you entered is already existed.</h1>
				<hr>
				<a href="https://www.passhield.com"><button>Login</button></a>
				<a href="https://www.passhield.com/register"><button>Register</button></a>
			  </body>
			</html>
			`))
			fmt.Println(err)
		} else {
			//init the salt_with_attempt
			salt_with_attempt[saltKey] = maxAttempts

			//test only
			fmt.Println("Salt: ", salt)
			fmt.Println("Salted byte: ", salting)
			fmt.Println("hmac: ", hmac)

			//w.Write([]byte(fmt.Sprintf("register success")))
			w.Write([]byte(`<!DOCTYPE html>
			<html>
			  <head>
				<meta charset='utf-8'>
				<title>register successful</title>
			  </head>
			  <body>
				<h1>Congratulations, your registration is successful!</h1>
				<hr>
				<p>Thank you for registering with our website, you can now log in with your account and start using our services.</p>
				<a href="https://www.passhield.com"><button>Login</button></a>
			  </body>
			</html>
			`))
			//test only
			//w.Write([]byte(fmt.Sprintf("username: %s", username)))
			//w.Write([]byte(fmt.Sprintf("hmac: %s ", hmac)))
			//w.Write([]byte(fmt.Sprintf("salt: %s ", salt)))

		}
	})

	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Only GET requests are allowed", http.StatusBadRequest)
			return
		}

		err := r.ParseForm()
		if err != nil {
			http.Error(w, "Failed to parse request body", http.StatusBadRequest)
			return
		}

		username := r.FormValue("username")
		pwd := r.FormValue("password")

		fmt.Printf("📫 %v sent username %v\n", r.RemoteAddr, username)
		fmt.Printf("📫 %v sent password %v\n", r.RemoteAddr, pwd)

		//Rate limiting

		salt, hmac, err := GetSaltAndHmac(username, database)

		if err != nil {
			if err == sql.ErrNoRows {
				fmt.Printf("username is not in the Database")
				w.WriteHeader(http.StatusNotFound)
				//w.Write([]byte(fmt.Sprintf("Record not found for username %s: . Error: %s", username, err.Error())))
				w.Write([]byte(`<!DOCTYPE html>
					<html>
					<head>
						<meta charset='utf-8'>
						<title>login failed</title>
					</head>
					<body>
						<h1>Sorry, login is failed.</h1>
						<h1>the username is not existed in database</h1>
						<hr>
						<a href="https://www.passhield.com"><button>Login</button></a>
						<a href="https://www.passhield.com/register"><button>Register</button></a>
					</body>
					</html>
				`))
			}
		} else {
			var salting = salting(pwd, salt)

			//test only
			fmt.Println("Salted byte: ", salting)
			fmt.Println("hmac from DB: ", hmac)

			var newHmac = genHmac(salting, hmacKey)

			//test only
			fmt.Println("newhmac: ", newHmac)

			//test only
			//w.Write([]byte(fmt.Sprintf("username: %s", username)))
			//w.Write([]byte(fmt.Sprintf("salting: %s ", salting)))
			if err := decrementAttempts(salt, salt_with_attempt); err != nil {
				fmt.Println(err)
				resetAttempts(salt_with_attempt, resetTime, maxAttempts)
			} else {
				if login := compareHMACs(hmac, newHmac); login == true {
					fmt.Println("Verification success")
					//test only
					//w.Write([]byte(fmt.Sprintf("Verification success")))

					//sent token
					random_token, err := GenerateRandomString(256)
					if err != nil {
						fmt.Println(err)
					} else {
						//save token in DB
						if err := AddToken(username, random_token, database); err != nil {
							fmt.Println(err)
						} else {
							w.Write([]byte(fmt.Sprintf(random_token)))
						}
					}
				} else {
					fmt.Println("Verification failure")
					//test only
					//w.Write([]byte(fmt.Sprintf("Verification failure")))
					w.Write([]byte(`<!DOCTYPE html>
						<html>
						<head>
							<meta charset='utf-8'>
							<title>login failed</title>
						</head>
						<body>
							<h1>Sorry, login is failed.</h1>
							<h1>password is not matched with your username</h1>
							<hr>
							<a href="https://www.passhield.com"><button>Login</button></a>
							<a href="https://www.passhield.com/register"><button>Register</button></a>
						</body>
						</html>
					`))
				}
			}
		}
	})

	//Test only
	http.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if err := shutdown(hmacKey, database, salt_with_attempt, resetTime); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("the state information successfully")
		}
	})

	tlsCfg := tls.Config{
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{cert},
				PrivateKey:  priv,
			},
		},
	}

	server := http.Server{Addr: serverAddr, TLSConfig: &tlsCfg}
	fmt.Printf("📎 Token now available under https://%s/token\n", serverAddr)
	fmt.Printf("👂 Listening on https://%s/secret for secrets...\n", serverAddr)
	err = server.ListenAndServeTLS("", "")
	fmt.Println(err)
}

func AddToken(username string, Token string, database *sql.DB) error {
	var count int
	row := database.QueryRow("SELECT COUNT(*) FROM Token WHERE username = ?", username)
	if err := row.Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		//delete all the row from table Token
		statement, err := database.Prepare("DELETE FROM Token WHERE username = ?")
		if err != nil {
			return err
		}
		_, err = statement.Exec(username)
		if err != nil {
			return err
		}
	}
	statement, err := database.Prepare("INSERT INTO Token (username, Token) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	_, err = statement.Exec(username, Token)
	return err
}

// resetAttempts resets the number of attempts for each salt in the given attempts
// map to the maximum number of attempts and updates the resetTime to be one day
// later if the current time is after the resetTime.
//
// Parameters:
//   - attempts: a map that contains the number of attempts for each salt
//   - resetTime: a pointer to a time.Time variable that stores the time when the
//     attempts were last reset
//   - attemptsmax: the maximum number of attempts allowed for each salt
func resetAttempts(attempts map[string]int, resetTime *time.Time, attemptsmax int) {
	now := time.Now()

	// If the current time is after the resetTime, reset the attempts for each salt
	if now.After(*resetTime) {
		for salt := range attempts {
			attempts[salt] = attemptsmax
		}

		// Update the resetTime to be one day later
		*resetTime = now.Add(time.Hour * 24)
	}
}

// decrementAttempts decrements the number of attempts associated with the given
// salt in the provided map salt_with_attempt. If the salt is not found in the map,
// an error is returned. If the number of attempts reaches zero, an error is also
// returned. Otherwise, the attempts counter is decremented by one and the function
// returns nil.
//
// Parameters:
//   - salt: a byte slice representing the salt to decrement attempts for.
//   - salt_with_attempt: a map[string]int where keys are salt values (as hex strings)
//     and values are the corresponding number of attempts.
//
// Returns:
// - an error if the salt is not found in the map or the number of attempts is zero.
// - nil otherwise.
func decrementAttempts(salt []byte, salt_with_attempt map[string]int) error {
	saltKey := fmt.Sprintf("%x", salt)
	attempt, ok := salt_with_attempt[saltKey]
	if !ok {
		return errors.New("salt not found in attempts map")
	}
	if attempt == 0 {
		return errors.New("no attempts left")
	}
	salt_with_attempt[saltKey] = attempt - 1
	return nil
}

// securely stores the state information outside the enclave when systeam is shutting down.
// input hmacKey return error if exits
func shutdown(hmacKey []byte, database *sql.DB, salt_with_attempt map[string]int, resetTime *time.Time) error {
	if err := stores_HmacKey(hmacKey, database); err != nil {
		fmt.Println(err)
	}
	if err := stores_salt_with_attempt(salt_with_attempt, resetTime, database); err != nil {
		return err
	}

	return err
}

func stores_salt_with_attempt(salt_with_attempt map[string]int, resetTime *time.Time, database *sql.DB) error {
	var count int
	row := database.QueryRow("SELECT COUNT(*) FROM salt_with_attempt")
	if err := row.Scan(&count); err != nil {
		return err
	}
	// first checks if an Hmac already exists
	//in the Sealed table of the provided database
	//and returns an error if it does.
	if count > 0 {
		//delete all the row from table salt_with_attempt
		statement, err := database.Prepare("DELETE FROM salt_with_attempt;")
		if err != nil {
			return err
		}
		_, err = statement.Exec()
		if err != nil {
			return err
		}

		//delete all the row from table salt_with_attempt
		statement, err = database.Prepare("DELETE FROM resetTime;")
		if err != nil {
			return err
		}
		_, err = statement.Exec()
		if err != nil {
			return err
		}
	}

	jsonData, err := json.Marshal(salt_with_attempt)
	if err != nil {
		return err
	}

	var Sealed_jsonData = sealing(jsonData)
	_, err = database.Exec("INSERT INTO salt_with_attempt (data) VALUES (?)", Sealed_jsonData)
	if err != nil {
		return err
	}

	statement, err := database.Prepare("INSERT INTO resetTime (time) VALUES (?)")
	if err != nil {
		return err
	}
	defer statement.Close()

	resetTimeStr := resetTime.Format(time.RFC3339) // Convert to ISO 8601 format
	var resetTime_byte = []byte(resetTimeStr)
	var Sealed_resetTime = sealing(resetTime_byte)
	_, err = statement.Exec(Sealed_resetTime)

	return err
}

func stores_HmacKey(hmacKey []byte, database *sql.DB) error {
	var count int
	row := database.QueryRow("SELECT COUNT(*) FROM Sealed")
	if err := row.Scan(&count); err != nil {
		return err
	}
	// first checks if an Hmac already exists
	//in the Sealed table of the provided database
	//and returns an error if it does.
	if count > 0 {
		return errors.New("Hmac already exists")
	}
	// If no Hmac exists, the function prepares an INSERT statement
	//to insert the hmacKey into the Sealed table and executes it.
	statement, err := database.Prepare("INSERT INTO Sealed (Hmackey) VALUES (?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	var Seal = sealing(hmacKey)
	_, err = statement.Exec(Seal)

	return err
}

// input hmac from DB and newhmac return bool
// compare HMACs if hmac1 = hmac2 return true
// Otherwise return false
func compareHMACs(hmac1, hmac2 string) bool {
	byteHMAC1 := []byte(hmac1)
	byteHMAC2 := []byte(hmac2)

	// Compare the length of two HMAC values to see if they are equal
	if len(byteHMAC1) != len(byteHMAC2) {
		return false
	}

	// Use the subtle.ConstantTimeCompare() function to compare two HMAC values for equality
	//The ConstantTimeCompare() function compares two byte arrays to see if they are equal
	//but it takes time to execute independent of the size of the two inputs
	//thus preventing side channel attacks.
	return subtle.ConstantTimeCompare(byteHMAC1, byteHMAC2) == 1
}

// Determine if username already exists in the
// database if not add the three inputs to the database
// if it does return an error message
func AddSaltAndHmac(username string, hmac string, salt []byte, database *sql.DB) error {
	var count int
	row := database.QueryRow("SELECT COUNT(*) FROM Hmac WHERE username = ?", username)
	if err := row.Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("username already exists")
	}
	statement, err := database.Prepare("INSERT INTO Hmac (username, hmac, salt) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	_, err = statement.Exec(username, hmac, salt)
	return err
}

// input username return salt and hmac.
// Determine if username already exists in the database
// if not  return an error message if it does return  salt and hmac.
func GetSaltAndHmac(username string, database *sql.DB) ([]byte, string, error) {
	// Execute the selection statement salt and HMAC value
	rows, err := database.Query("SELECT hmac, salt FROM Hmac WHERE username = ?", username)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var salt []byte
	var hmac string

	// Traversing the query results, store the salt and HMAC values into the variable
	for rows.Next() {
		err = rows.Scan(&hmac, &salt)
		if err != nil {
			return nil, "", err
		}
	}

	if salt == nil || hmac == "" {
		return nil, "", sql.ErrNoRows
	}

	return salt, hmac, nil
}

// generate a random hmac key and seal it
func initialize(database *sql.DB) ([]byte, map[string]int, *time.Time, error) {
	var count int
	row := database.QueryRow("SELECT COUNT(*) FROM Sealed")
	if err := row.Scan(&count); err != nil {
		return nil, nil, nil, err
	}
	if count > 0 {
		// Execute the selection statement Hmackey value
		rows, err := database.Query("SELECT Hmackey FROM Sealed")
		if err != nil {
			return nil, nil, nil, err
		}
		defer rows.Close()
		var Seal []byte

		// Traversing the query results, store the salt and HMAC values into the variable
		for rows.Next() {
			err = rows.Scan(&Seal)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		if Seal == nil {
			return nil, nil, nil, err
		}
		var hmac = Unseal(Seal)

		//test only
		//fmt.Printf("init() unSeal hmac key: %s", hmac)

		var jsonData []byte
		err = database.QueryRow("SELECT data FROM salt_with_attempt").Scan(&jsonData)
		if err != nil {
			return nil, nil, nil, err
		}
		var UnSeal_jsonData = Unseal(jsonData)
		var saltWithAttempt map[string]int
		err = json.Unmarshal(UnSeal_jsonData, &saltWithAttempt)
		if err != nil {
			return nil, nil, nil, err
		}

		var timeBytes []byte
		err = database.QueryRow("SELECT time FROM resetTime").Scan(&timeBytes)
		if err != nil {
			return nil, nil, nil, err
		}
		var UnSeal_time = Unseal(timeBytes)
		resetTimeStr := string(UnSeal_time)
		resetTime, err := time.Parse(time.RFC3339, resetTimeStr)
		if err != nil {
			return nil, nil, nil, err
		}

		return hmac, saltWithAttempt, &resetTime, err
	} else {
		//generate a random hmac key
		random_hmackey, err := GenerateRandomString(128)
		if err != nil {
			return nil, nil, nil, err
		}
		fmt.Println("random_hmackey: ", random_hmackey)
		fmt.Println("*********************************************************************************************************************************************************************************************************")
		var keyBytes = []byte(random_hmackey)

		salt_with_attempt := make(map[string]int)
		resetTime := time.Now().Add(time.Minute * 1)

		return keyBytes, salt_with_attempt, &resetTime, err
	}
}

func GenerateRandomString(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"
	ret := make([]byte, n)
	for i := 0; i < n; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}
		ret[i] = letters[num.Int64()]
	}

	return string(ret), nil
}

// seal the Hmac key
func sealing(hmacKey []byte) []byte {
	var additionalData []byte
	seal, err := ecrypto.SealWithUniqueKey(hmacKey, additionalData)

	if err != nil {
		panic(err)
	}
	return seal
}

// Unseal the Hmac key
func Unseal(hmacKey []byte) []byte {
	var additionalData []byte
	hmac_key, err := ecrypto.Unseal(hmacKey, additionalData)

	if err != nil {
		panic(err)
	}
	return hmac_key
}

func generateRandomSalt(saltSize int) []byte {
	var salt = make([]byte, saltSize)

	_, err := rand.Read(salt[:])

	if err != nil {
		panic(err)
	}

	return salt
}

func salting(password string, salt []byte) []byte {
	var passwordBytes = []byte(password)
	passwordBytes = append(passwordBytes, salt...)
	return passwordBytes
}

func genHmac(salted_password []byte, key []byte) string {

	// Create sha-256 hasher
	mac := hmac.New(sha256.New, key)

	// Write password bytes to the hasher
	mac.Write(salted_password)

	// Get the SHA-256 hashed password
	expectedMAC := mac.Sum(nil)

	// Convert the hashed password to a hex string
	var hmacHex = hex.EncodeToString(expectedMAC)

	//return hmacHex
	return hmacHex
}

func createCertificate() ([]byte, crypto.PrivateKey) {
	template := &x509.Certificate{
		SerialNumber: &big.Int{},
		Subject:      pkix.Name{CommonName: "localhost"},
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	cert, _ := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	return cert, priv
}

func checkTokenExpiration(ctx context.Context, tokenString string, cert []byte) {
	//ticker := time.NewTicker(480 * time.Minute)
	ticker := time.NewTicker(8 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Token expiration checker stopped.")
			return
		case <-ticker.C:
			tokenTmp, err := jwt.ParseSigned(tokenString)
			if err != nil {
				fmt.Printf("Failed to parse token: %v\n", err)
				continue
			}

			claims := jwt.Claims{}
			err = tokenTmp.UnsafeClaimsWithoutVerification(&claims)
			if err != nil {
				fmt.Printf("Failed to extract claims: %v\n", err)
				continue
			}

			expirationTime := time.Unix(int64(*claims.Expiry), 0)
			if time.Now().After(expirationTime) {
				// Token expired
				fmt.Println("Token has expired. Renewing token...")

				newToken, err := enclave.CreateAzureAttestationToken(cert, attestationProviderURL)
				if err != nil {
					fmt.Printf("Failed to renew token: %v\n", err)
					continue
				}

				token = newToken
				fmt.Println("Token renewed successfully.")
			} else {
				fmt.Println("Token is still valid.")
			}
		}
	}
}
