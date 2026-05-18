//go:build linux && webkit2_41

package main

/*
#cgo webkit2_41 pkg-config: webkit2gtk-4.1

#include <webkit2/webkit2.h>

// patchWebViewRecursive walks the widget tree recursively looking for a
// WebKitWebView. When found, enables enable-media-stream and reloads the
// page so the JS context is recreated with RTCPeerConnection available.
// Returns 1 if patched, 0 if no WebView found at this node.
static int patchWebViewRecursive(GtkWidget *widget) {
    if (WEBKIT_IS_WEB_VIEW(widget)) {
        WebKitSettings *settings = webkit_web_view_get_settings(WEBKIT_WEB_VIEW(widget));
        webkit_settings_set_enable_media_stream(settings, TRUE);
        webkit_settings_set_enable_media_capabilities(settings, TRUE);
        // Reload so the JS context is recreated with the new settings.
        // This re-fires window.onload → EventsOn re-registers → RequestLoopbackOffer
        // re-emits any cached offers. The reload is self-healing.
        webkit_web_view_reload(WEBKIT_WEB_VIEW(widget));
        return 1;
    }
    if (GTK_IS_CONTAINER(widget)) {
        GList *children = gtk_container_get_children(GTK_CONTAINER(widget));
        for (GList *c = children; c != NULL; c = c->next) {
            if (patchWebViewRecursive(GTK_WIDGET(c->data))) {
                g_list_free(children);
                return 1;
            }
        }
        g_list_free(children);
    }
    return 0;
}

// tryPatchWebRTC is a GLib timeout callback that runs ON THE GTK MAIN THREAD.
// GTK is single-threaded — calling gtk_window_list_toplevels, WEBKIT_IS_WEB_VIEW,
// etc. from a Go goroutine causes silent failures (NULL returns, widget tree
// corruption) because the Go goroutine is NOT the GTK main loop thread.
// Using g_timeout_add ensures this runs on the correct thread.
//
// Returns TRUE to keep retrying, FALSE to stop.
static gboolean tryPatchWebRTC(gpointer data) {
    int *attempts = (int *)data;
    (*attempts)++;

    GList *toplevels = gtk_window_list_toplevels();
    for (GList *l = toplevels; l != NULL; l = l->next) {
        if (patchWebViewRecursive(GTK_WIDGET(l->data))) {
            g_list_free(toplevels);
            g_printerr("[bcr_client] EnableWebRTC: patched on GTK main thread (attempt %d)\n", *attempts);
            free(data);
            return FALSE; // stop timer — success
        }
    }
    g_list_free(toplevels);

    // Give up after 150 attempts (150 × 100ms = 15 seconds).
    if (*attempts >= 150) {
        g_printerr("[bcr_client] EnableWebRTC: WARNING — WebView not found after %d attempts\n", *attempts);
        free(data);
        return FALSE; // stop timer — exhausted
    }
    return TRUE; // keep retrying
}

// scheduleWebRTCPatch dispatches a repeating 100ms timer onto the GTK main
// loop that will search for the WebKitWebView and patch its settings.
// Safe to call from any thread — g_timeout_add is thread-safe and the
// callback runs on the GTK main thread.
static void scheduleWebRTCPatch() {
    int *attempts = calloc(1, sizeof(int));
    g_timeout_add(100, tryPatchWebRTC, attempts);
}
*/
import "C"

import "log"

// EnableWebRTC schedules a WebKit settings patch on the GTK main thread.
// The patch enables "enable-media-stream" (which provides RTCPeerConnection)
// and reloads the page so the JS context picks it up.
//
// This MUST use g_timeout_add to run on the GTK main thread — calling GTK
// functions from a Go goroutine causes silent failures because GTK is
// single-threaded.
func EnableWebRTC() {
	C.scheduleWebRTCPatch()
	log.Println("[bcr_client] EnableWebRTC: patch scheduled on GTK main thread (will retry for up to 15s)")
}
