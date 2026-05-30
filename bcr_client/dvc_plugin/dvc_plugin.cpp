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

#include <atomic>

class CDVCChannelCallback : public IWTSVirtualChannelCallback {
private:
    ULONG m_cRef;
    IWTSVirtualChannel* m_pChannel;
    SOCKET m_socket;
    std::thread m_socketThread;
    std::atomic<bool> m_bClosed;

public:
    CDVCChannelCallback(IWTSVirtualChannel* pChannel) 
        : m_cRef(1), m_pChannel(pChannel), m_socket(INVALID_SOCKET), m_bClosed(false) {
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

    void StartTCPBridgeThread() {
        m_socketThread = std::thread([this]() {
            WSADATA wsaData;
            if (WSAStartup(MAKEWORD(2,2), &wsaData) != 0) {
                OutputDebugStringA("BCR DLL: WSAStartup failed in bridge thread.");
                m_pChannel->Close();
                return;
            }

            sockaddr_in addr = {0};
            addr.sin_family = AF_INET;
            addr.sin_port = htons(LOCAL_PORT);
            inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);

            while (!m_bClosed) {
                m_socket = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
                if (m_socket == INVALID_SOCKET) {
                    OutputDebugStringA("BCR DLL: Socket creation failed in bridge thread.");
                    break;
                }

                OutputDebugStringA("BCR DLL: Connecting to 127.0.0.1:8081...");
                if (connect(m_socket, (sockaddr*)&addr, sizeof(addr)) != SOCKET_ERROR) {
                    OutputDebugStringA("BCR DLL: Bidirectional TCP connection established with local player!");
                    break;
                }

                // Close socket for retry
                closesocket(m_socket);
                m_socket = INVALID_SOCKET;

                // Cooldown retry delay (1 second) checking m_bClosed
                for (int i = 0; i < 10 && !m_bClosed; i++) {
                    Sleep(100);
                }
            }

            if (m_bClosed || m_socket == INVALID_SOCKET) {
                if (m_socket != INVALID_SOCKET) {
                    closesocket(m_socket);
                    m_socket = INVALID_SOCKET;
                }
                m_pChannel->Close();
                return;
            }

            // TCP connected. Forward TCP to DVC.
            std::vector<char> buffer(65536);
            while (!m_bClosed) {
                int bytesRead = recv(m_socket, buffer.data(), (int)buffer.size(), 0);
                if (bytesRead <= 0) {
                    OutputDebugStringA("BCR DLL: TCP connection lost/closed.");
                    break;
                }
                m_pChannel->Write((ULONG)bytesRead, (BYTE*)buffer.data(), NULL);
            }

            CloseConnections();
            m_pChannel->Close();
        });
    }

    // IUnknown
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppvObject) override {
        if (!ppvObject) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(IWTSVirtualChannelCallback)) {
            *ppvObject = this;
            AddRef();
            return S_OK;
        }
        *ppvObject = NULL;
        return E_NOINTERFACE;
    }

    ULONG STDMETHODCALLTYPE AddRef() override {
        return InterlockedIncrement(&m_cRef);
    }

    ULONG STDMETHODCALLTYPE Release() override {
        ULONG res = InterlockedDecrement(&m_cRef);
        if (res == 0) delete this;
        return res;
    }

    // IWTSVirtualChannelCallback
    HRESULT STDMETHODCALLTYPE OnDataReceived(ULONG cbSize, BYTE* pBuffer) override {
        if (m_socket != INVALID_SOCKET) {
            int sent = send(m_socket, (const char*)pBuffer, (int)cbSize, 0);
            if (sent == SOCKET_ERROR) {
                CloseConnections();
                return E_FAIL;
            }
        }
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE OnClose() override {
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
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppvObject) override {
        if (!ppvObject) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(IWTSListenerCallback)) {
            *ppvObject = this;
            AddRef();
            return S_OK;
        }
        *ppvObject = NULL;
        return E_NOINTERFACE;
    }

    ULONG STDMETHODCALLTYPE AddRef() override {
        return InterlockedIncrement(&m_cRef);
    }

    ULONG STDMETHODCALLTYPE Release() override {
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
    ) override {
        OutputDebugStringA("BCR DLL: OnNewChannelConnection triggered!");
        *pbAccept = FALSE;
        *ppCallback = NULL;

        CDVCChannelCallback* pChanCallback = new CDVCChannelCallback(pChannel);
        pChanCallback->StartTCPBridgeThread();

        OutputDebugStringA("BCR DLL: Accepting channel and starting background TCP retry bridge.");
        *pbAccept = TRUE;
        *ppCallback = pChanCallback;
        pChanCallback->AddRef();

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

// Exported entrypoint used by RDP DVC client LoadLibrary activation: VirtualChannelGetInstance
extern "C" __declspec(dllexport) HRESULT __stdcall VirtualChannelGetInstance(
    REFIID requestedIid,
    ULONG* pNumObjs,
    VOID** ppObjArray
) {
    OutputDebugStringA("BCR DLL: VirtualChannelGetInstance called!");
    if (!pNumObjs) return E_POINTER;

    // We expose exactly one object: IWTSPlugin
    if (!ppObjArray) {
        *pNumObjs = 1;
        return S_OK;
    }

    if (*pNumObjs < 1) return E_INVALIDARG;

    OutputDebugStringA("BCR DLL: Instantiating CDVCPlugin...");
    CDVCPlugin* pPlugin = new CDVCPlugin();
    
    void* out = nullptr;
    HRESULT hr = pPlugin->QueryInterface(requestedIid, &out);
    pPlugin->Release(); // balance local ref

    if (SUCCEEDED(hr)) {
        OutputDebugStringA("BCR DLL: IWTSPlugin instance created and returned successfully!");
        ppObjArray[0] = out;
    } else {
        char szBuf[128];
        wsprintfA(szBuf, "BCR DLL: QueryInterface failed with hr=0x%08X", (unsigned int)hr);
        OutputDebugStringA(szBuf);
    }
    return hr;
}
