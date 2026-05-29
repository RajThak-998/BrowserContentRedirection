#ifdef _stdcall
#undef _stdcall
#endif
#define _stdcall __stdcall

#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>
#include <tsvirtualchannels.h>
#include <cchannel.h>
#include <thread>
#include <vector>

#ifdef VCAPITYPE
#undef VCAPITYPE
#endif
#define VCAPITYPE __stdcall

// Define the missing RDP DVC function pointers and structures for MinGW compatibility
typedef HRESULT (VCAPITYPE * PVIRTUALCHANNELINITLISTENEREX)(
    PCHANNEL_ENTRY_POINTS pEntryPoints,
    IUnknown* pListener
);

typedef struct tagCHANNEL_ENTRY_POINTS_EX {
    DWORD                            cbSize;
    DWORD                            dwVersion;
    PVOID                            pInitHandle;
    PVIRTUALCHANNELINIT              pVirtualChannelInit;
    PVIRTUALCHANNELOPEN              pVirtualChannelOpen;
    PVIRTUALCHANNELCLOSE             pVirtualChannelClose;
    PVIRTUALCHANNELWRITE             pVirtualChannelWrite;
    PVOID                            pReserved;
    PVIRTUALCHANNELINITLISTENEREX    pVirtualChannelInitListenerEx;
} CHANNEL_ENTRY_POINTS_EX, *PCHANNEL_ENTRY_POINTS_EX;

#pragma comment(lib, "ws2_32.lib")

#define CHANNEL_NAME "BCR_VC"
#define LOCAL_PORT 8081

class CDVCChannelCallback : public IWTSVirtualChannelCallback {
private:
    ULONG m_cRef;
    IWTSVirtualChannel* m_pChannel;
    SOCKET m_socket;
    std::thread m_socketThread;
    bool m_bClosed;

public:
    CDVCChannelCallback(IWTSVirtualChannel* pChannel) : m_cRef(1), m_pChannel(pChannel), m_socket(INVALID_SOCKET), m_bClosed(false) {
        m_pChannel->AddRef();
    }

    ~CDVCChannelCallback() {
        CloseConnections();
        m_pChannel->Release();
    }

    void CloseConnections() {
        m_bClosed = true;
        if (m_socket != INVALID_SOCKET) {
            closesocket(m_socket);
            m_socket = INVALID_SOCKET;
        }
        if (m_socketThread.joinable()) {
            m_socketThread.join();
        }
    }

    void StartSocketReader() {
        m_socketThread = std::thread([this]() {
            std::vector<char> buffer(65536);
            while (!m_bClosed) {
                int bytesRead = recv(m_socket, buffer.data(), (int)buffer.size(), 0);
                if (bytesRead <= 0) {
                    break;
                }
                m_pChannel->Write((ULONG)bytesRead, (BYTE*)buffer.data(), NULL);
            }
            m_pChannel->Close();
        });
    }

    HRESULT InitializeTCP() {
        WSADATA wsaData;
        if (WSAStartup(MAKEWORD(2,2), &wsaData) != 0) return E_FAIL;

        m_socket = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
        if (m_socket == INVALID_SOCKET) return E_FAIL;

        sockaddr_in addr = {0};
        addr.sin_family = AF_INET;
        addr.sin_port = htons(LOCAL_PORT);
        inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);

        if (connect(m_socket, (sockaddr*)&addr, sizeof(addr)) == SOCKET_ERROR) {
            closesocket(m_socket);
            m_socket = INVALID_SOCKET;
            return E_FAIL;
        }

        StartSocketReader();
        return S_OK;
    }

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppvObject) {
        if (!ppvObject) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(IWTSVirtualChannelCallback)) {
            *ppvObject = this;
            AddRef();
            return S_OK;
        }
        *ppvObject = NULL;
        return E_NOINTERFACE;
    }

    ULONG STDMETHODCALLTYPE AddRef() {
        return InterlockedIncrement(&m_cRef);
    }

    ULONG STDMETHODCALLTYPE Release() {
        ULONG res = InterlockedDecrement(&m_cRef);
        if (res == 0) delete this;
        return res;
    }

    // IWTSVirtualChannelCallback
    HRESULT STDMETHODCALLTYPE OnDataReceived(ULONG cbSize, BYTE* pBuffer) {
        if (m_socket != INVALID_SOCKET) {
            int sent = send(m_socket, (const char*)pBuffer, (int)cbSize, 0);
            if (sent == SOCKET_ERROR) {
                CloseConnections();
                return E_FAIL;
            }
        }
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE OnClose() {
        CloseConnections();
        return S_OK;
    }
};

class CDVCListenerCallback : public IWTSListenerCallback {
private:
    ULONG m_cRef;

public:
    CDVCListenerCallback() : m_cRef(1) {}

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppvObject) {
        if (!ppvObject) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(IWTSListenerCallback)) {
            *ppvObject = this;
            AddRef();
            return S_OK;
        }
        *ppvObject = NULL;
        return E_NOINTERFACE;
    }

    ULONG STDMETHODCALLTYPE AddRef() {
        return InterlockedIncrement(&m_cRef);
    }

    ULONG STDMETHODCALLTYPE Release() {
        ULONG res = InterlockedDecrement(&m_cRef);
        if (res == 0) delete this;
        return res;
    }

    // IWTSListenerCallback
    HRESULT STDMETHODCALLTYPE OnNewChannelConnection(
        IWTSVirtualChannel* pChannel,
        BSTR pData,
        BOOL* pbAccept,
        IWTSVirtualChannelCallback** ppCallback
    ) {
        *pbAccept = FALSE;
        *ppCallback = NULL;

        CDVCChannelCallback* pChanCallback = new CDVCChannelCallback(pChannel);
        if (pChanCallback->InitializeTCP() == S_OK) {
            *pbAccept = TRUE;
            *ppCallback = pChanCallback;
            pChanCallback->AddRef();
        }
        pChanCallback->Release();
        return S_OK;
    }
};

class CDVCPlugin : public IWTSPlugin {
private:
    ULONG m_cRef;

public:
    CDVCPlugin() : m_cRef(1) {}

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppvObject) {
        if (!ppvObject) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(IWTSPlugin)) {
            *ppvObject = this;
            AddRef();
            return S_OK;
        }
        *ppvObject = NULL;
        return E_NOINTERFACE;
    }

    ULONG STDMETHODCALLTYPE AddRef() {
        return InterlockedIncrement(&m_cRef);
    }

    ULONG STDMETHODCALLTYPE Release() {
        ULONG res = InterlockedDecrement(&m_cRef);
        if (res == 0) delete this;
        return res;
    }

    // IWTSPlugin
    HRESULT STDMETHODCALLTYPE Initialize(IWTSVirtualChannelManager* pChannelMgr) {
        CDVCListenerCallback* pListenerCallback = new CDVCListenerCallback();
        IWTSListener* pListener = NULL;
        HRESULT hr = pChannelMgr->CreateListener(CHANNEL_NAME, 0, pListenerCallback, &pListener);
        if (SUCCEEDED(hr)) {
            pListener->Release();
        }
        pListenerCallback->Release();
        return hr;
    }

    HRESULT STDMETHODCALLTYPE Connected() {
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE Disconnected(DWORD dwReason) {
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE Terminated() {
        return S_OK;
    }
};

// Exported initialization function
extern "C" __declspec(dllexport) HRESULT VCAPITYPE VirtualChannelEntry(
    PCHANNEL_ENTRY_POINTS pEntryPoints
) {
    if (!pEntryPoints) return E_INVALIDARG;

    // Check size of CHANNEL_ENTRY_POINTS to ensure it has EX fields
    if (pEntryPoints->cbSize < sizeof(CHANNEL_ENTRY_POINTS_EX)) {
        return E_FAIL;
    }

    PCHANNEL_ENTRY_POINTS_EX pEntryPointsEx = (PCHANNEL_ENTRY_POINTS_EX)pEntryPoints;
    if (!pEntryPointsEx->pVirtualChannelInitListenerEx) {
        return E_FAIL;
    }

    CDVCPlugin* pPlugin = new CDVCPlugin();
    HRESULT hr = pEntryPointsEx->pVirtualChannelInitListenerEx(pEntryPoints, pPlugin);
    pPlugin->Release();
    return hr;
}
