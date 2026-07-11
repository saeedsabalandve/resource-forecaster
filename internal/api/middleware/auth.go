package middleware

import (
    "context"
    "crypto/subtle"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/golang-jwt/jwt/v5"
    "github.com/google/uuid"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"

    "resource-forecaster/internal/config"
)

// # Context keys for request-scoped values
type contextKey string

const (
    ContextKeyClaims    contextKey = "jwt-claims"
    ContextKeyRequestID contextKey = "request-id"
    ContextKeyTenantID  contextKey = "tenant-id"
)

// # CustomClaims extends JWT standard claims with service-specific fields
type CustomClaims struct {
    jwt.RegisteredClaims
    TenantID  string   `json:"tenant_id"`
    Roles     []string `json:"roles"`
    Permissions []string `json:"permissions"`
    ServiceAccount string `json:"service_account,omitempty"`
}

// # AuthMiddleware provides multi-layer authentication:
// # 1. JWT Bearer tokens (user/service authentication)
// # 2. API Key (internal service communication)
// # 3. mTLS (infrastructure-level, handled by service mesh)
type AuthMiddleware struct {
    jwtSecret       []byte
    apiKeys         map[string]string  // # key hash -> service name
    tokenExpiration time.Duration
    issuer          string
}

// # NewAuthMiddleware initializes authentication with production security
func NewAuthMiddleware(cfg config.AuthConfig) *AuthMiddleware {
    am := &AuthMiddleware{
        jwtSecret:       []byte(cfg.JWTSecret),
        apiKeys:         make(map[string]string),
        tokenExpiration: cfg.JWTExpiration,
        issuer:          "resource-forecaster",
    }

    // # Load and hash API keys for constant-time comparison
    for _, apiKey := range cfg.InternalAPIKeys {
        // # Store SHA-256 hash instead of raw key
        keyHash := hashAPIKey(apiKey)
        am.apiKeys[keyHash] = "internal-service"
    }

    return am
}

// # Authenticate is the main middleware handler
func (am *AuthMiddleware) Authenticate(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        tracer := otel.Tracer("auth-middleware")
        ctx, span := tracer.Start(r.Context(), "authenticate")
        defer span.End()

        // # Generate request ID if not present
        requestID := r.Header.Get("X-Request-ID")
        if requestID == "" {
            requestID = uuid.New().String()
        }
        ctx = context.WithValue(ctx, ContextKeyRequestID, requestID)
        span.SetAttributes(attribute.String("request_id", requestID))

        // # Try authentication methods in order of preference
        authErr := am.authenticateRequest(ctx, r)
        
        if authErr != nil {
            span.RecordError(authErr)
            span.SetStatus(codes.Error, authErr.Error())
            
            log.Warn().
                Err(authErr).
                Str("request_id", requestID).
                Str("method", r.Method).
                Str("path", r.URL.Path).
                Str("remote_addr", r.RemoteAddr).
                Msg("Authentication failed")

            http.Error(w, `{"error":"authentication_failed","message":"Invalid or expired credentials"}`, 
                http.StatusUnauthorized)
            return
        }

        span.SetStatus(codes.Ok, "authenticated")
        
        // # Add security headers
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "DENY")
        w.Header().Set("X-XSS-Protection", "1; mode=block")
        w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
        w.Header().Set("X-Request-ID", requestID)

        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

// # authenticateRequest tries multiple authentication methods
func (am *AuthMiddleware) authenticateRequest(ctx context.Context, r *http.Request) error {
    // # 1. Try JWT Bearer token
    authHeader := r.Header.Get("Authorization")
    if strings.HasPrefix(authHeader, "Bearer ") {
        token := strings.TrimPrefix(authHeader, "Bearer ")
        return am.validateJWT(ctx, token, r)
    }

    // # 2. Try API Key
    apiKey := r.Header.Get("X-API-Key")
    if apiKey != "" {
        return am.validateAPIKey(ctx, apiKey)
    }

    // # 3. Check for service mesh mTLS headers (Istio/Linkerd)
    if r.Header.Get("X-Forwarded-Client-Cert") != "" {
        return am.validateMTLS(ctx, r)
    }

    return fmt.Errorf("no valid authentication credentials provided")
}

// # validateJWT performs comprehensive JWT validation
func (am *AuthMiddleware) validateJWT(ctx context.Context, tokenString string, r *http.Request) error {
    // # Parse and validate JWT with multiple verification steps
    token, err := jwt.ParseWithClaims(tokenString, &CustomClaims{}, 
        func(token *jwt.Token) (interface{}, error) {
            // # Verify signing algorithm
            if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
                return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
            }
            return am.jwtSecret, nil
        },
        jwt.WithLeeway(30*time.Second),  // # 30 second clock skew tolerance
        jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
    )
    if err != nil {
        return fmt.Errorf("jwt validation failed: %w", err)
    }

    claims, ok := token.Claims.(*CustomClaims)
    if !ok || !token.Valid {
        return fmt.Errorf("invalid token claims")
    }

    // # Validate issuer
    if claims.Issuer != am.issuer {
        return fmt.Errorf("invalid token issuer: %s", claims.Issuer)
    }

    // # Check if token is expired
    if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(time.Now()) {
        return fmt.Errorf("token expired at %v", claims.ExpiresAt.Time)
    }

    // # Check if token is used before its not-before time
    if claims.NotBefore != nil && claims.NotBefore.Time.After(time.Now()) {
        return fmt.Errorf("token not valid before %v", claims.NotBefore.Time)
    }

    // # Store claims in context for downstream use
    r = r.WithContext(context.WithValue(r.Context(), ContextKeyClaims, claims))
    if claims.TenantID != "" {
        r = r.WithContext(context.WithValue(r.Context(), ContextKeyTenantID, claims.TenantID))
    }

    log.Debug().
        Str("subject", claims.Subject).
        Str("tenant_id", claims.TenantID).
        Strs("roles", claims.Roles).
        Msg("JWT authentication successful")

    return nil
}

// # validateAPIKey uses constant-time comparison to prevent timing attacks
func (am *AuthMiddleware) validateAPIKey(ctx context.Context, apiKey string) error {
    keyHash := hashAPIKey(apiKey)
    
    // # Constant-time comparison against all stored API keys
    for storedHash, serviceName := range am.apiKeys {
        if subtle.ConstantTimeCompare([]byte(keyHash), []byte(storedHash)) == 1 {
            log.Debug().
                Str("service", serviceName).
                Msg("API key authentication successful")
            return nil
        }
    }

    return fmt.Errorf("invalid API key")
}

// # validateMTLS handles service mesh certificate-based authentication
func (am *AuthMiddleware) validateMTLS(ctx context.Context, r *http.Request) error {
    // # Extract SPIFFE ID from X-Forwarded-Client-Cert header (Istio)
    clientCert := r.Header.Get("X-Forwarded-Client-Cert")
    
    // # In production, this would validate against a SPIFFE trust domain
    if strings.Contains(clientCert, "URI=spiffe://") {
        log.Debug().
            Str("spiffe_id", clientCert).
            Msg("mTLS authentication successful")
        return nil
    }

    return fmt.Errorf("invalid mTLS certificate")
}

// # RequireRole middleware for role-based access control
func (am *AuthMiddleware) RequireRole(roles ...string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            claims, ok := r.Context().Value(ContextKeyClaims).(*CustomClaims)
            if !ok {
                http.Error(w, `{"error":"forbidden","message":"No claims found"}`, 
                    http.StatusForbidden)
                return
            }

            // # Check if user has any of the required roles
            for _, requiredRole := range roles {
                for _, userRole := range claims.Roles {
                    if userRole == requiredRole {
                        next.ServeHTTP(w, r)
                        return
                    }
                }
            }

            log.Warn().
                Str("subject", claims.Subject).
                Strs("user_roles", claims.Roles).
                Strs("required_roles", roles).
                Msg("Insufficient permissions")

            http.Error(w, `{"error":"forbidden","message":"Insufficient permissions"}`, 
                http.StatusForbidden)
        })
    }
}

// # RequirePermission checks specific granular permissions
func (am *AuthMiddleware) RequirePermission(permissions ...string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            claims, ok := r.Context().Value(ContextKeyClaims).(*CustomClaims)
            if !ok {
                http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
                return
            }

            for _, required := range permissions {
                for _, userPerm := range claims.Permissions {
                    if userPerm == required || userPerm == "admin" {
                        next.ServeHTTP(w, r)
                        return
                    }
                }
            }

            http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
        })
    }
}

// # GenerateToken creates a new JWT token for service accounts
func (am *AuthMiddleware) GenerateToken(serviceAccount string, tenantID string, roles []string) (string, error) {
    now := time.Now()
    claims := CustomClaims{
        RegisteredClaims: jwt.RegisteredClaims{
            Issuer:    am.issuer,
            Subject:   serviceAccount,
            Audience:  jwt.ClaimStrings{"resource-forecaster"},
            ExpiresAt: jwt.NewNumericDate(now.Add(am.tokenExpiration)),
            NotBefore: jwt.NewNumericDate(now),
            IssuedAt:  jwt.NewNumericDate(now),
            ID:        uuid.New().String(),
        },
        TenantID:       tenantID,
        Roles:          roles,
        ServiceAccount: serviceAccount,
    }

    token := jwt.NewWithClaims(jwt.SigningMethodHS512, claims)
    tokenString, err := token.SignedString(am.jwtSecret)
    if err != nil {
        return "", fmt.Errorf("failed to sign token: %w", err)
    }

    return tokenString, nil
}

// # hashAPIKey creates SHA-256 hash for secure API key storage
func hashAPIKey(key string) string {
    h := sha256.New()
    h.Write([]byte(key))
    return fmt.Sprintf("%x", h.Sum(nil))
}
