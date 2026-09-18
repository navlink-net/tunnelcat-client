// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// app_window_linux.c — GTK/WebKit2GTK implementation for the Tunnel Cat app window.
// Compiled as a separate CGO source file so definitions appear exactly once.
// (CGO inlines Go-file C preambles into multiple generated .c files, causing
// multiple-definition link errors for any non-static symbol defined there.)

#include <webkit2/webkit2.h>
#include <stdlib.h>

// Go-exported callbacks — declared here to avoid depending on _cgo_export.h path.
extern void goAppWinConnect(void);
extern void goAppWinDisconnect(void);
extern void goAppWinSettings(char *json);
extern void goAppWinPageReady(void);
extern void goAppWinKey(char *key);
extern void goAppWinHaveKeyAnswer(int hasKey);
extern void goAppWinLogUploadPref(int enabled);
extern void goAppWinCredentialLogin(char *json);
extern void goAppWinKeyModeSwitch(void);
extern void goAppWinRecommend(char *username);
extern void goAppWinClubThemePreview(char *theme);
extern void goAppWinLoadFailed(char *message);
extern void goAppWinProcessTerminated(int reason);

// ── Static GTK/WebKit widget pointers (single-instance window) ────────────────

static GtkWidget     *g_app_win = NULL;
static WebKitWebView *g_app_wv  = NULL;

// ── Signal handlers (GTK main thread) ────────────────────────────────────────

static void on_wv_connect(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)r; (void)d;
	goAppWinConnect();
}
static void on_wv_disconnect(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)r; (void)d;
	goAppWinDisconnect();
}
static void on_wv_page_ready(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)r; (void)d;
	goAppWinPageReady();
}
static void on_wv_settings(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	char *json = jsc_value_to_json(v, 0);
	if (json) { goAppWinSettings(json); g_free(json); }
}
static void on_wv_key(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	char *str = jsc_value_to_string(v);
	if (str) { goAppWinKey(str); g_free(str); }
}
static void on_wv_recommend(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	char *str = jsc_value_to_string(v);
	if (str) { goAppWinRecommend(str); g_free(str); }
}
static void on_wv_club_theme_preview(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	char *str = jsc_value_to_string(v);
	if (str) { goAppWinClubThemePreview(str); g_free(str); }
}
static void on_wv_have_key_answer(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	goAppWinHaveKeyAnswer(jsc_value_to_boolean(v) ? 1 : 0);
}
static void on_wv_log_upload_pref(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	goAppWinLogUploadPref(jsc_value_to_boolean(v) ? 1 : 0);
}
static void on_wv_credential_login(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)d;
	JSCValue *v = webkit_javascript_result_get_js_value(r);
	char *json = jsc_value_to_json(v, 0);
	if (json) { goAppWinCredentialLogin(json); g_free(json); }
}
static void on_wv_key_mode_switch(WebKitUserContentManager *m, WebKitJavascriptResult *r, gpointer d) {
	(void)m; (void)r; (void)d;
	goAppWinKeyModeSwitch();
}

static gboolean on_app_win_delete(GtkWidget *w, GdkEvent *e, gpointer d) {
	(void)e; (void)d;
	gtk_widget_hide(w);
	return TRUE;
}

// on_wv_load_failed and on_wv_process_terminated exist purely so a real
// load or renderer-process failure is ever logged at all --
// webkit_web_view_load_html is called below with a NULL GError sink and had
// no failure signal wired up before this, so either kind of failure was
// completely silent (see the 2026-08-16 gray-window support reports this
// followed from -- that specific bug turned out to be an accelerated-
// compositing rendering defect, not a load/process failure, so these
// handlers wouldn't have caught it, but the total absence of any failure
// visibility here was a separate, real gap worth closing on its own).
static gboolean on_wv_load_failed(WebKitWebView *wv, WebKitLoadEvent event,
                                   gchar *failing_uri, GError *error, gpointer d) {
	(void)wv; (void)event; (void)d;
	goAppWinLoadFailed(error && error->message ? error->message : failing_uri);
	return FALSE; // let WebKit's default handling continue
}
static void on_wv_process_terminated(WebKitWebView *wv, WebKitWebProcessTerminationReason reason, gpointer d) {
	(void)wv; (void)d;
	goAppWinProcessTerminated((int)reason);
}

// ── g_idle_add callbacks ──────────────────────────────────────────────────────

static gboolean app_win_create_idle(gpointer data) {
	char *html = (char*)data;

	WebKitUserContentManager *ucm = webkit_user_content_manager_new();
	webkit_user_content_manager_register_script_message_handler(ucm, "sncConnect");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncDisconnect");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncSetSettings");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncPageReady");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncKey");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncHaveKeyAnswer");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncSetLogUploadPref");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncCredentialLogin");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncKeyModeSwitch");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncRecommend");
	webkit_user_content_manager_register_script_message_handler(ucm, "sncClubThemePreview");
	g_signal_connect(ucm, "script-message-received::sncConnect",     G_CALLBACK(on_wv_connect),    NULL);
	g_signal_connect(ucm, "script-message-received::sncDisconnect",  G_CALLBACK(on_wv_disconnect), NULL);
	g_signal_connect(ucm, "script-message-received::sncSetSettings", G_CALLBACK(on_wv_settings),   NULL);
	g_signal_connect(ucm, "script-message-received::sncPageReady",   G_CALLBACK(on_wv_page_ready), NULL);
	g_signal_connect(ucm, "script-message-received::sncKey",         G_CALLBACK(on_wv_key),        NULL);
	g_signal_connect(ucm, "script-message-received::sncHaveKeyAnswer",  G_CALLBACK(on_wv_have_key_answer),  NULL);
	g_signal_connect(ucm, "script-message-received::sncSetLogUploadPref", G_CALLBACK(on_wv_log_upload_pref), NULL);
	g_signal_connect(ucm, "script-message-received::sncCredentialLogin", G_CALLBACK(on_wv_credential_login), NULL);
	g_signal_connect(ucm, "script-message-received::sncKeyModeSwitch", G_CALLBACK(on_wv_key_mode_switch), NULL);
	g_signal_connect(ucm, "script-message-received::sncRecommend",     G_CALLBACK(on_wv_recommend),       NULL);
	g_signal_connect(ucm, "script-message-received::sncClubThemePreview", G_CALLBACK(on_wv_club_theme_preview), NULL);

	// Force software rendering. Belt-and-suspenders alongside the
	// WEBKIT_DISABLE_COMPOSITING_MODE env var set in tray_main_linux.go's
	// runTrayProcess (which some newer webkit2gtk versions ignore in favor
	// of this settings API) -- see that comment for the failure this works
	// around: on some Xubuntu/XFCE machines the accelerated compositor
	// partially fails, leaving image layers rendered but text/interactive
	// layers and pointer input dead, with no error surfaced anywhere.
	WebKitSettings *wk_settings = webkit_settings_new();
	webkit_settings_set_hardware_acceleration_policy(wk_settings, WEBKIT_HARDWARE_ACCELERATION_POLICY_NEVER);

	g_app_wv = WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW,
		"user-content-manager", ucm, "settings", wk_settings, NULL));
	g_object_unref(ucm);
	g_object_unref(wk_settings);
	g_signal_connect(g_app_wv, "load-failed", G_CALLBACK(on_wv_load_failed), NULL);
	g_signal_connect(g_app_wv, "web-process-terminated", G_CALLBACK(on_wv_process_terminated), NULL);

	g_app_win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	gtk_window_set_title(GTK_WINDOW(g_app_win), "Tunnel Cat");
	gtk_window_set_default_size(GTK_WINDOW(g_app_win), 440, 520);
	gtk_window_set_resizable(GTK_WINDOW(g_app_win), FALSE);
	gtk_container_add(GTK_CONTAINER(g_app_win), GTK_WIDGET(g_app_wv));
	g_signal_connect(g_app_win, "delete-event", G_CALLBACK(on_app_win_delete), NULL);

	webkit_web_view_load_html(g_app_wv, html, NULL);
	gtk_widget_show_all(g_app_win);
	free(html);
	return G_SOURCE_REMOVE;
}

static gboolean app_win_show_idle(gpointer data) {
	(void)data;
	if (g_app_win) gtk_window_present(GTK_WINDOW(g_app_win));
	return G_SOURCE_REMOVE;
}

static gboolean app_win_run_js_idle(gpointer data) {
	char *js = (char*)data;
	if (g_app_wv) webkit_web_view_evaluate_javascript(g_app_wv, js, -1, NULL, NULL, NULL, NULL, NULL);
	free(js);
	return G_SOURCE_REMOVE;
}

// ── Public API called from Go ─────────────────────────────────────────────────

int app_win_is_created(void) { return g_app_win != NULL ? 1 : 0; }

void schedule_app_win_create(gpointer data) { g_idle_add(app_win_create_idle, data); }
void schedule_app_win_show(void)            { g_idle_add(app_win_show_idle, NULL);   }
void schedule_app_win_run_js(gpointer data) { g_idle_add(app_win_run_js_idle, data); }
