package main

import (
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	echologrus "github.com/davrux/echo-logrus/v4"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
	ddEcho "gopkg.in/DataDog/dd-trace-go.v1/contrib/labstack/echo.v4"
	"gorm.io/gorm"
)

//go:embed public/*
var embeddedAssets embed.FS

//go:embed views/*
var embeddedViews embed.FS

type TemplateRegistry struct {
	templates map[string]*template.Template
}

// Implement e.Renderer interface
func (t *TemplateRegistry) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	tmpl, ok := t.templates[name]
	if !ok {
		err := errors.New("Template not found -> " + name)
		return err
	}
	return tmpl.ExecuteTemplate(w, "layout.html", data)
}

func (svc *Service) RegisterSharedRoutes(e *echo.Echo) {

	templates := make(map[string]*template.Template)
	templates["apps/index.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/index.html", "views/layout.html"))
	templates["apps/new.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/new.html", "views/layout.html"))
	templates["apps/show.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/show.html", "views/layout.html"))
	templates["apps/create.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/create.html", "views/layout.html"))
	templates["alby/index.html"] = template.Must(template.ParseFS(embeddedViews, "views/backends/alby/index.html", "views/layout.html"))
	templates["about.html"] = template.Must(template.ParseFS(embeddedViews, "views/about.html", "views/layout.html"))
	templates["404.html"] = template.Must(template.ParseFS(embeddedViews, "views/404.html", "views/layout.html"))
	templates["lnd/index.html"] = template.Must(template.ParseFS(embeddedViews, "views/backends/lnd/index.html", "views/layout.html"))
	e.Renderer = &TemplateRegistry{
		templates: templates,
	}
	e.HideBanner = true
	e.Use(echologrus.Middleware())

	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup: "form:_csrf",
	}))
	e.Use(session.Middleware(sessions.NewCookieStore([]byte(svc.cfg.CookieSecret))))
	e.Use(ddEcho.Middleware(ddEcho.WithServiceName("nostr-wallet-connect")))

	assetSubdir, _ := fs.Sub(embeddedAssets, "public")
	assetHandler := http.FileServer(http.FS(assetSubdir))
	e.GET("/public/*", echo.WrapHandler(http.StripPrefix("/public/", assetHandler)))
	e.GET("/apps", svc.AppsListHandler)
	e.GET("/apps/new", svc.AppsNewHandler)
	e.GET("/apps/:pubkey", svc.AppsShowHandler)
	e.POST("/apps", svc.AppsCreateHandler)
	e.POST("/apps/delete/:pubkey", svc.AppsDeleteHandler)
	e.GET("/logout", svc.LogoutHandler)
	e.GET("/about", svc.AboutHandler)
	e.GET("/", svc.IndexHandler)
}

func (svc *Service) IndexHandler(c echo.Context) error {
	sess, _ := session.Get(CookieName, c)
	returnTo := sess.Values["return_to"]
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user != nil && returnTo != nil {
		delete(sess.Values, "return_to")
		sess.Options.MaxAge = 0
		sess.Options.SameSite = http.SameSiteLaxMode
		if svc.cfg.CookieDomain != "" {
			sess.Options.Domain = svc.cfg.CookieDomain
		}
		sess.Save(c.Request(), c.Response())
		return c.Redirect(302, fmt.Sprintf("%s", returnTo))
	}
	if user != nil {
		return c.Redirect(302, "/apps")
	}
	return c.Render(http.StatusOK, fmt.Sprintf("%s/index.html", strings.ToLower(svc.cfg.LNBackendType)), map[string]interface{}{})
}

func (svc *Service) AboutHandler(c echo.Context) error {
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "about.html", map[string]interface{}{
		"User": user,
	})
}

func (svc *Service) AppsListHandler(c echo.Context) error {
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user == nil {
		return c.Redirect(302, "/")
	}

	apps := user.Apps

	lastEvents := make(map[uint]NostrEvent)
	eventsCounts := make(map[uint]int64)
	for _, app := range apps {
		var lastEvent NostrEvent
		var eventsCount int64
		svc.db.Where("app_id = ?", app.ID).Order("id desc").Limit(1).Find(&lastEvent)
		svc.db.Model(&NostrEvent{}).Where("app_id = ?", app.ID).Count(&eventsCount)
		lastEvents[app.ID] = lastEvent
		eventsCounts[app.ID] = eventsCount
	}

	return c.Render(http.StatusOK, "apps/index.html", map[string]interface{}{
		"Apps":         apps,
		"User":         user,
		"LastEvents":   lastEvents,
		"EventsCounts": eventsCounts,
	})
}

func (svc *Service) AppsShowHandler(c echo.Context) error {
	csrf, _ := c.Get(middleware.DefaultCSRFConfig.ContextKey).(string)
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user == nil {
		return c.Redirect(302, "/")
	}

	app := App{}
	svc.db.Where("user_id = ? AND nostr_pubkey = ?", user.ID, c.Param("pubkey")).First(&app)

	if app.NostrPubkey == "" {
		return c.Render(http.StatusNotFound, "404.html", map[string]interface{}{
			"User": user,
		})
	}

	lastEvent := NostrEvent{}
	svc.db.Where("app_id = ?", app.ID).Order("id desc").Limit(1).Find(&lastEvent)
	var eventsCount int64
	svc.db.Model(&NostrEvent{}).Where("app_id = ?", app.ID).Count(&eventsCount)

	paySpecificPermission := AppPermission{}
	appPermissions := []AppPermission{}
	expiresAt := time.Time{}
	svc.db.Where("app_id = ?", app.ID).Find(&appPermissions)

	requestMethods := []string{}
	for _, appPerm := range appPermissions {
		if expiresAt.IsZero() && !appPerm.ExpiresAt.IsZero() {
			expiresAt = appPerm.ExpiresAt
		}
		if appPerm.RequestMethod == NIP_47_PAY_INVOICE_METHOD {
			//find the pay_invoice-specific permissions
			paySpecificPermission = appPerm
		}
		requestMethods = append(requestMethods, nip47MethodDescriptions[appPerm.RequestMethod])
	}

	expiresAtFormatted := expiresAt.Format("January 2, 2006 03:04 PM")

	renewsIn := ""
	budgetUsage := int64(0)
	maxAmount := paySpecificPermission.MaxAmount
	if maxAmount > 0 {
		budgetUsage = svc.GetBudgetUsage(&paySpecificPermission)
		endOfBudget := GetEndOfBudget(paySpecificPermission.BudgetRenewal, app.CreatedAt)
		renewsIn = getEndOfBudgetString(endOfBudget)
	}

	return c.Render(http.StatusOK, "apps/show.html", map[string]interface{}{
		"App":                   app,
		"PaySpecificPermission": paySpecificPermission,
		"RequestMethods":        requestMethods,
		"ExpiresAt":             expiresAt,
		"ExpiresAtFormatted":    expiresAtFormatted,
		"User":                  user,
		"LastEvent":             lastEvent,
		"EventsCount":           eventsCount,
		"BudgetUsage":           budgetUsage,
		"RenewsIn":              renewsIn,
		"Csrf":                  csrf,
	})
}

func getEndOfBudgetString(endOfBudget time.Time) (result string) {
	if endOfBudget.IsZero() {
		return "--"
	}
	endOfBudgetDuration := endOfBudget.Sub(time.Now())

	//less than a day
	if endOfBudgetDuration.Hours() < 24 {
		hours := int(endOfBudgetDuration.Hours())
		minutes := int(endOfBudgetDuration.Minutes()) % 60
		return fmt.Sprintf("%d hours and %d minutes", hours, minutes)
	}
	//less than a month
	if endOfBudgetDuration.Hours() < 24*30 {
		days := int(endOfBudgetDuration.Hours() / 24)
		return fmt.Sprintf("%d days", days)
	}
	//more than a month
	months := int(endOfBudgetDuration.Hours() / 24 / 30)
	days := int(endOfBudgetDuration.Hours()/24) % 30
	if days > 0 {
		return fmt.Sprintf("%d months %d days", months, days)
	}
	return fmt.Sprintf("%d months", months)
}

func (svc *Service) AppsNewHandler(c echo.Context) error {
	appName := c.QueryParam("name")
	if (appName == "") {
		 // c - for client (deprecated)
		appName = c.QueryParam("c")
	}
	pubkey := c.QueryParam("pubkey")
	returnTo := c.QueryParam("return_to")
	maxAmount := c.QueryParam("max_amount")
	budgetRenewal := strings.ToLower(c.QueryParam("budget_renewal"))
	expiresAt := c.QueryParam("expires_at") // YYYY-MM-DD or MM/DD/YYYY or timestamp in seconds
	if expiresAtTimestamp, err := strconv.Atoi(expiresAt); err == nil {
		expiresAt = time.Unix(int64(expiresAtTimestamp), 0).Format(time.RFC3339)
	}
	expiresAtISO, _ := time.Parse(time.RFC3339, expiresAt)
	expiresAtFormatted := expiresAtISO.Format("January 2, 2006 03:04 PM")

	requestMethods := c.QueryParam("request_methods")
	customRequestMethods := requestMethods
	if requestMethods == "" {
		// if no request methods are given, enable them all by default
		keys := []string{}
		for key := range nip47MethodDescriptions {
			keys = append(keys, key)
		}

		requestMethods = strings.Join(keys, " ")
	}
	csrf, _ := c.Get(middleware.DefaultCSRFConfig.ContextKey).(string)

	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user == nil {
		sess, _ := session.Get(CookieName, c)
		sess.Values["return_to"] = c.Path() + "?" + c.QueryString()
		sess.Options.MaxAge = 0
		sess.Options.SameSite = http.SameSiteLaxMode
		if svc.cfg.CookieDomain != "" {
			sess.Options.Domain = svc.cfg.CookieDomain
		}
		sess.Save(c.Request(), c.Response())
		return c.Redirect(302, fmt.Sprintf("/%s/auth?c=%s", strings.ToLower(svc.cfg.LNBackendType), appName))
	}

	//construction to return a map with all possible permissions
	//and indicate which ones are checked by default in the front-end
	type RequestMethodHelper struct {
		Description string
		Icon        string
		Checked     bool
	}

	requestMethodHelper := map[string]*RequestMethodHelper{}
	for k, v := range nip47MethodDescriptions {
		requestMethodHelper[k] = &RequestMethodHelper{
			Description: v,
			Icon:        nip47MethodIcons[k],    
		}
	}

	for _, m := range strings.Split(requestMethods, " ") {
		if _, ok := nip47MethodDescriptions[m]; ok {
			requestMethodHelper[m].Checked = true
		}
	}

	return c.Render(http.StatusOK, "apps/new.html", map[string]interface{}{
		"User":                 user,
		"Name":                 appName,
		"Pubkey":               pubkey,
		"ReturnTo":             returnTo,
		"MaxAmount":            maxAmount,
		"BudgetRenewal":        budgetRenewal,
		"ExpiresAt":            expiresAt,
		"ExpiresAtFormatted":   expiresAtFormatted,
		"RequestMethods":       requestMethods,
		"CustomRequestMethods": customRequestMethods,
		"RequestMethodHelper":  requestMethodHelper,
		"Csrf":                 csrf,
	})
}

func (svc *Service) AppsCreateHandler(c echo.Context) error {
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user == nil {
		return c.Redirect(302, "/")
	}

	name := c.FormValue("name")
	var pairingPublicKey string
	var pairingSecretKey string
	if c.FormValue("pubkey") == "" {
		pairingSecretKey = nostr.GeneratePrivateKey()
		pairingPublicKey, _ = nostr.GetPublicKey(pairingSecretKey)
	} else {
		pairingPublicKey = c.FormValue("pubkey")
		//validate public key
		decoded, err := hex.DecodeString(pairingPublicKey)
		if err != nil || len(decoded) != 32 {
			svc.Logger.Errorf("Invalid public key format: %s", pairingPublicKey)
			return c.Redirect(302, "/apps")
		}
	}
	app := App{Name: name, NostrPubkey: pairingPublicKey}
	maxAmount, _ := strconv.Atoi(c.FormValue("MaxAmount"))
	budgetRenewal := c.FormValue("BudgetRenewal")

	expiresAt := time.Time{}
	if c.FormValue("ExpiresAt") != "" {
		expiresAt, err = time.Parse(time.RFC3339, c.FormValue("ExpiresAt"))
		if (err != nil) {
			return fmt.Errorf("Invalid ExpiresAt: %v", err)
		}
	}

	if !expiresAt.IsZero() {
		expiresAt = time.Date(expiresAt.Year(), expiresAt.Month(), expiresAt.Day(), 23, 59, 59, 0, expiresAt.Location())
	}

	err = svc.db.Transaction(func(tx *gorm.DB) error {
		err = tx.Model(&user).Association("Apps").Append(&app)
		if err != nil {
			return err
		}

		requestMethods := c.FormValue("RequestMethods")
		if requestMethods == "" {
			return fmt.Errorf("Won't create an app without request methods.")
		}
		//request methods should be space separated list of known request kinds
		methodsToCreate := strings.Split(requestMethods, " ")
		for _, m := range methodsToCreate {
			//if we don't know this method, we return an error
			if _, ok := nip47MethodDescriptions[m]; !ok {
				return fmt.Errorf("Did not recognize request method: %s", m)
			}
			appPermission := AppPermission{
				App:           app,
				RequestMethod: m,
				ExpiresAt:     expiresAt,
				//these fields are only relevant for pay_invoice
				MaxAmount:     maxAmount,
				BudgetRenewal: budgetRenewal,
			}
			err = tx.Create(&appPermission).Error
			if err != nil {
				return err
			}
		}
		// commit transaction
		return nil
	})

	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"pairingPublicKey": pairingPublicKey,
			"name":             name,
		}).Errorf("Failed to save app: %v", err)
		return c.Redirect(302, "/apps")
	}

	// FIXME: need a public-facing relay URL env variable
	// and fallback to svc.cfg.Relay (hosted instance uses an internal URL)
	publicRelayUrl := "wss://relay.getalby.com/v1";

	if c.FormValue("returnTo") != "" {
		returnToUrl, err := url.Parse(c.FormValue("returnTo"))
		if err == nil {
			query := returnToUrl.Query()
			query.Add("relay", publicRelayUrl);
			query.Add("pubkey", svc.cfg.IdentityPubkey)
			if user.LightningAddress != "" {
				query.Add("lud16", user.LightningAddress)
			}
			returnToUrl.RawQuery = query.Encode()
			return c.Redirect(302, returnToUrl.String())
		}
	}

	var lud16 string
	if user.LightningAddress != "" {
		lud16 = fmt.Sprintf("&lud16=%s", user.LightningAddress)
	}
	pairingUri := template.URL(fmt.Sprintf("nostr+walletconnect://%s?relay=%s&secret=%s%s", svc.cfg.IdentityPubkey, publicRelayUrl, pairingSecretKey, lud16))
	return c.Render(http.StatusOK, "apps/create.html", map[string]interface{}{
		"User":          user,
		"PairingUri":    pairingUri,
		"PairingSecret": pairingSecretKey,
		"Pubkey":        pairingPublicKey,
		"Name":          name,
	})
}

func (svc *Service) AppsDeleteHandler(c echo.Context) error {
	user, err := svc.GetUser(c)
	if err != nil {
		return err
	}
	if user == nil {
		return c.Redirect(302, "/")
	}
	app := App{}
	svc.db.Where("user_id = ? AND nostr_pubkey = ?", user.ID, c.Param("pubkey")).First(&app)
	svc.db.Delete(&app)
	return c.Redirect(302, "/apps")
}

func (svc *Service) LogoutHandler(c echo.Context) error {
	sess, _ := session.Get(CookieName, c)
	sess.Options.MaxAge = -1
	if svc.cfg.CookieDomain != "" {
		sess.Options.Domain = svc.cfg.CookieDomain
	}
	sess.Save(c.Request(), c.Response())
	return c.Redirect(302, "/")
}
