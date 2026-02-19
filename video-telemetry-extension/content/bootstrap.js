(()=>{
    function start() {
        console.log("[Bootstrap] BCR Video Telemetry starting...");
        VideoRegistry.init();
        console.log("[Bootstrap] Registry initialized. Tracking active.");
    }

    function stop() {
        console.log("[Bootstrap] Page unloading. Tearing down...");
        OverlayRenderer.destroyAll();   
        VideoRegistry.destroy();
    }

    if (
        document.readyState === "complete" || 
        document.readyState === "interactive"
    ) {
        start();
    } else {
        document.addEventListener("DOMContentLoaded", start, {once: true});
    }

    window.addEventListener("pagehide", stop, {once: true});
})();