#include <atomic>
#include <mutex>
#include <thread>
#include <fstream>
#include <queue>
#include <sstream>
#include <nlohmann/json.hpp>
#include "icvar.h"
#include "cdll_interfaces.h"
#ifdef _WIN32
#define SERVER_LIB_PATH "\\csgo\\bin\\win64\\server.dll"
#else
#include <dlfcn.h>
#include <sys/mman.h>
#define SERVER_LIB_PATH "/csgo/bin/linuxsteamrt64/libserver.so"
#define PAGESIZE 4096
#endif

// IDKW registering a cmd on Linux makes the game process exit with a non zero code (Segmentation fault)
#ifdef _WIN32
#define CON_COMMAND_ENABLED 1
#endif

using nlohmann::json;
using std::string;

void* GetLibAddress(void* lib, const char* name) {
#if defined _WIN32
    return GetProcAddress((HMODULE)lib, name);
#else
    return dlsym(lib, name);
#endif
}

char* GetLastErrorString() {
#ifdef _WIN32
    DWORD error = GetLastError();
    static char s[_MAX_U64TOSTR_BASE2_COUNT];
    sprintf(s, "%lu", error);

    return s;
#else
    return dlerror();
#endif
}

void* LoadLib(const char* path) {
#ifdef _WIN32
    return LoadLibrary(path);
#else
    return dlopen(path, RTLD_NOW);
#endif
}

struct Action {
    int tick;
    string cmd;
};

struct Sequence {
    std::vector<Action> actions;
};

typedef bool (*AppSystemConnectFn)(IAppSystem* appSystem, CreateInterfaceFn factory);
typedef void (*AppSystemShutdownFn)();
typedef void (*FrameStageNotifyFn)(void* thisptr, ClientFrameStage_t curStage);
typedef void (*ClientFullyConnectFn)(void* thisptr, int playerSlot);

CreateInterfaceFn factory = NULL;
AppSystemConnectFn serverConfigConnect = NULL;
ClientFullyConnectFn originalClientFullyConnect = NULL;
AppSystemShutdownFn serverConfigShutdown = NULL;
CreateInterfaceFn serverCreateInterface = NULL;
ISource2EngineToClient* engineToClient = NULL;
ISource2Client* client = NULL;
FrameStageNotifyFn originalFrameStageNotify = NULL;
ICvar* g_pCVar = NULL;
string gameInfoPath;
string gameInfoBackupPath;
const char* demoPath = NULL;
bool isPlayingDemo = false;
std::atomic<int> currentTick{-1};
std::atomic<bool> isQuitting{false};
int lastPauseTick = -1;
std::mutex sequencesMutex;
std::queue<Sequence> sequences;
std::mutex pendingCommandsMutex;
std::queue<std::string> pendingCommands;
std::chrono::steady_clock::time_point startTime = std::chrono::steady_clock::now();

void LogToFile(const char* pMsg) {
    FILE* pFile = fopen("dem-render.log", "a");
    if (pFile == NULL)
    {
        return;
    }

    fprintf(pFile, "%s\n", pMsg);
    fclose(pFile);
}

void DeleteLogFile()
{
    remove("dem-render.log");
}

void Log(const char *msg, ...)
{
    va_list args;
    va_start(args, msg);
    char buf[1024] = {};
    vsnprintf(buf, sizeof(buf), msg, args);
    ConColorMsg(Color(227, 0, 255, 255), "DEM RENDER: %s\n", buf);
    va_end(args);
    LogToFile(buf);
}

void PluginError(const char* msg, ...)
{
    va_list args;
    va_start(args, msg);
    char buf[1024] = {};
    vsnprintf(buf, sizeof(buf), msg, args);
    va_end(args);

    // Since the "Armory" update, calling Plat_FatalErrorFunc crashes the game on Windows.
    #ifdef _WIN32
        Plat_MessageBox("Error", buf);
        Plat_ExitProcess(1);
    #else
        Plat_FatalErrorFunc("%s", buf);
    #endif
}

inline bool FileExists(const std::string& name) {
    std::ifstream f(name.c_str());

    return f.good();
}

static void UnhideCommandsAndCvars()
{
    uint64 flagsToRemove = (FCVAR_HIDDEN | FCVAR_DEVELOPMENTONLY);

    ConCommandData* data = g_pCVar->GetConCommandData(ConCommandRef());
    for (ConCommandRef concmd = ConCommandRef((uint16)0); concmd.GetRawData() != data; concmd = ConCommandRef(concmd.GetAccessIndex() + 1))
    {
        if (concmd.GetFlags() & flagsToRemove)
        {
            concmd.RemoveFlags(flagsToRemove);
        }
    }

    for (ConVarRefAbstract ref(ConVarRef((uint16)0)); ref.IsValidRef(); ref = ConVarRefAbstract(ConVarRef(ref.GetAccessIndex() + 1)))
    {
        if (ref.GetFlags() & flagsToRemove)
        {
            ref.RemoveFlags(flagsToRemove);
        }
    }
}

// PatchVTableEntry overwrites a single vtable slot with newFunc, toggling page
// protection around the write. Used to hook engine/client virtual functions.
void PatchVTableEntry(void** vtable, int index, void* newFunc) {
#ifdef _WIN32
    size_t protectSize = sizeof(void*) * (index + 1);
    DWORD oldProtect = 0;
    if (!VirtualProtect(vtable, protectSize, PAGE_EXECUTE_READWRITE, &oldProtect))
    {
        PluginError("VirtualProtect PAGE_EXECUTE_READWRITE failed: %d", GetLastError());
    }
    vtable[index] = newFunc;
    DWORD ignore = 0;
    if (!VirtualProtect(vtable, protectSize, oldProtect, &ignore))
    {
        PluginError("VirtualProtect restore failed: %d", GetLastError());
    }
#else
    // Use the page containing the slot we're writing, not the page containing the table's base.
    void* slotAddr = (void*)&vtable[index];
    void* pageStart = (void*)((uintptr_t)slotAddr & ~(PAGESIZE - 1));
    if (mprotect(pageStart, PAGESIZE, PROT_READ | PROT_WRITE | PROT_EXEC) != 0)
    {
        PluginError("mprotect failed: %s", strerror(errno));
    }
    vtable[index] = newFunc;
    if (mprotect(pageStart, PAGESIZE, PROT_READ | PROT_EXEC) != 0)
    {
        PluginError("mprotect restore failed: %s", strerror(errno));
    }
#endif
}

// QueueEngineCommand schedules a console command to run on the engine thread in the
// next FrameStageNotify(FRAME_START). Engine commands are not safe to call from any
// other thread, so all ExecuteClientCmd calls funnel through the engine thread.
void QueueEngineCommand(const std::string& cmd) {
    std::lock_guard<std::mutex> lock(pendingCommandsMutex);
    pendingCommands.push(cmd);
}

ISource2EngineToClient* GetEngine()
{
    if (engineToClient != NULL) {
        return engineToClient;
    }

    if (factory == NULL) {
        return NULL;
    }

    engineToClient = (ISource2EngineToClient*)factory("Source2EngineToClient001", NULL);

    return engineToClient;
}

void RestoreGameinfoFile() {
    std::ifstream filebackupFile(gameInfoBackupPath);
    if (!filebackupFile.good()) {
        Log("gameinfo.gi backup file doesn't exist");
        filebackupFile.close();
        return;
    }

    std::ofstream destination(gameInfoPath);
    destination << filebackupFile.rdbuf();

    filebackupFile.close();
    destination.close();

    int result = remove(gameInfoBackupPath.c_str());
    if (result == 0) {
        Log("Backup file deleted successfully");
    }
    else
    {
        Log("Error deleting backup file");
    }
}

void LoadSequencesFile(string demoPath) {
    std::lock_guard<std::mutex> lock(sequencesMutex);
    sequences = {};

    string demoJsonPath = demoPath + ".json";
    if (FileExists(demoJsonPath)) {
        Log("Loading JSON file %s",  demoJsonPath.c_str());
        std::ifstream jsonFile(demoJsonPath);
        json jsonSequences = json::parse(jsonFile);

        std::istringstream stream(jsonSequences.dump(2));
        string line;
        while (std::getline(stream, line)) {
            Log("%s", line.c_str());
        }

        if (jsonSequences.size() == 0) {
            Log("No sequences found in JSON file");
            return;
        }

        Log("Loading %d sequences", jsonSequences.size());
        for (auto jsonSequence : jsonSequences) {
            Sequence sequence;
            for (auto jsonAction : jsonSequence["actions"]) {
                Action action;
                action.tick = jsonAction["tick"];
                action.cmd = jsonAction["cmd"];
                sequence.actions.push_back(action);
            }
            sequences.push(sequence);
            Log("Sequence with %d actions loaded", sequence.actions.size());
        }

        Log("%d sequences loaded", sequences.size());
    }
    else {
        Log("JSON sequences file not found at %s", demoJsonPath.c_str());
    }
}

// NewFrameStageNotify runs on the engine main thread. All demo playback control and
// console commands are issued here — calling engine commands from another thread is not
// thread safe and crashes CS2 (notably a demo_gototick issued right after endmovie from
// a background thread). This replaces the old background PlaybackLoop thread.
void NewFrameStageNotify(void* thisptr, ClientFrameStage_t stage)
{
    if (stage != ClientFrameStage_t::FRAME_START || isQuitting) {
        originalFrameStageNotify(thisptr, stage);
        return;
    }

    auto engine = GetEngine();
    if (engine == NULL) {
        Log("Engine interface not found");
        originalFrameStageNotify(thisptr, stage);
        return;
    }

    // Drain commands queued from other contexts (e.g. setup commands from ClientFullyConnect).
    {
        std::lock_guard<std::mutex> lock(pendingCommandsMutex);
        while (!pendingCommands.empty()) {
            std::string cmd = pendingCommands.front();
            pendingCommands.pop();
            Log("Executing queued command: %s", cmd.c_str());
            engine->ExecuteClientCmd(0, cmd.c_str(), true);
        }
    }

    // Workaround to start demo playback when Steam is in offline mode: the +playdemo launch
    // option doesn't work in that case, so force the command once a few seconds after launch.
    if (demoPath != NULL) {
        auto now = std::chrono::steady_clock::now();
        auto secondsSinceStart = std::chrono::duration_cast<std::chrono::seconds>(now - startTime).count();
        if (!engine->IsPlayingDemo() && secondsSinceStart >= 8) {
            string cmd = "playdemo \"" + string(demoPath) + "\"";
            demoPath = NULL;
            Log("Force playing demo: %s", cmd.c_str());
            engine->ExecuteClientCmd(0, cmd.c_str(), true);
        }
    }

    auto demo = engine->GetDemoPlayer();
    if (demo == NULL) {
        originalFrameStageNotify(thisptr, stage);
        return;
    }

    int newTick = demo->GetDemoTick();
    bool newIsPlayingDemo = engine->IsPlayingDemo();
    if (newIsPlayingDemo && !isPlayingDemo) {
        Log("[%d] Demo playback started, sequences %d", newTick, sequences.size());
        currentTick = -1;
    }
    else if (!newIsPlayingDemo && isPlayingDemo) {
        Log("[%d] Demo playback stopped, sequences %d", newTick, sequences.size());
        currentTick = -1;
    }

    isPlayingDemo = newIsPlayingDemo;
    if (!isPlayingDemo) {
        originalFrameStageNotify(thisptr, stage);
        return;
    }

    {
        std::lock_guard<std::mutex> lock(sequencesMutex);
        if (newTick != currentTick && !sequences.empty()) {
            // Fire actions whose tick falls in the range (fromTick, newTick].
            // Using a range instead of an exact match catches ticks that were skipped
            // when the demo advances multiple game ticks in a single frame.
            // If the demo jumped backward (e.g. after demo_gototick, or an Oct-2025
            // pause/resume tick regression), treat the new position as the baseline so
            // already-executed actions don't re-fire.
            int fromTick = (currentTick >= 0 && newTick < currentTick) ? newTick - 1 : currentTick.load();

            Sequence& currentSequence = sequences.front();
            for (auto& action : currentSequence.actions) {
                if (action.tick <= fromTick || action.tick > newTick) {
                    continue;
                }

                if (action.cmd == "pause_playback") {
                    // Since an October 2025 CS2 update, the tick after executing demo_pause and then demo_resume may be "in the past".
                    // For example pausing the demo at tick 1000 and resuming it may result in the current tick being 998.
                    // To avoid pausing the demo indefinitely, we check if we already paused at this tick.
                    if (lastPauseTick != -1 && lastPauseTick == action.tick) {
                        lastPauseTick = -1;
                        continue;
                    }

                    lastPauseTick = action.tick;
                    Log("[%d] Pausing demo playback", newTick);
                    engine->ExecuteClientCmd(0, "demo_pause", true);
                    std::this_thread::sleep_for(std::chrono::milliseconds(2000));
                    Log("[%d] Resuming demo playback", newTick);
                    engine->ExecuteClientCmd(0, "demo_resume", true);
                }
                else if (action.cmd == "go_to_next_sequence") {
                    Log("[%d] Going to next sequence, remaining sequences: %d", newTick, sequences.size() - 1);
                    sequences.pop();
                    engine->ExecuteClientCmd(0, "demo_gototick 0", true);
                    currentTick = -1;
                    lastPauseTick = -1;
                    break;
                }
                else {
                    Log("[%d] Executing: %s", newTick, action.cmd.c_str());
                    engine->ExecuteClientCmd(0, action.cmd.c_str(), true);
                }
            }
        }
    }

    currentTick = newTick;

    originalFrameStageNotify(thisptr, stage);
}

bool Connect(IAppSystem* appSystem, CreateInterfaceFn factoryFn)
{
    factory = factoryFn;
    bool result = serverConfigConnect(appSystem, factory);

    g_pCVar = (ICvar*)factory("VEngineCvar007", NULL);
    // Required to make the spec_lock_to_accountid command working since the 25/04/2024 update - it looks like the command has been hidden.
    // Also required to use the startmovie command.
    UnhideCommandsAndCvars();
    #ifdef CON_COMMAND_ENABLED
        ConVar_Register();
    #endif

    RestoreGameinfoFile();

    return result;
}


void Shutdown()
{
    isQuitting = true;

    if (serverConfigShutdown != NULL) {
        serverConfigShutdown();
    }

    #ifdef CON_COMMAND_ENABLED
        ConVar_Unregister();
    #endif
}

void AssertInsecureParameterIsPresent()
{
    bool found = false;
    // Since the "Armory" update, calling CommandLine()->HasParm("-insecure") crashes the game when the parameter is not present.
    auto parameters = CommandLine()->GetParms();
    for (int i = 0; i < CommandLine()->ParmCount(); i++)
    {
        if (strcmp(parameters[i], "-insecure") == 0)
        {
            found = true;
            break;
        }
    }

    if (!found)
    {
        PluginError("dem-render plugin loaded without the -insecure launch option.\n\nAborting.");
    }
}

void NewClientFullyConnect(void* thisptr, int playerSlot)
{
    Log("ClientFullyConnect: playerSlot=%d", playerSlot);
    if (client != NULL) {
        originalClientFullyConnect(thisptr, playerSlot);
        return;
    }

    // Hook FrameStageNotify to run engine commands from the engine thread, since it's not
    // thread safe to call engine commands from another thread.
    client = (ISource2Client*)factory("Source2Client002", NULL);
    if (client != NULL) {
        Log("Hooking FrameStageNotify");
        auto vtable = *(void***)client;
        originalFrameStageNotify = (FrameStageNotifyFn)vtable[36];
        PatchVTableEntry(vtable, 36, (void*)&NewFrameStageNotify);
        Log("Hooked FrameStageNotify");
    }

    // Since the 23/05/2024 CS2 update, the demo playback UI is displayed by default.
    // We set demo_ui_mode to 0 before playback starts to prevent the UI from being displayed.
    QueueEngineCommand("demo_ui_mode 0");
    QueueEngineCommand("sv_cheats 1"); // required to unlock commands such as startmovie

    originalClientFullyConnect(thisptr, playerSlot);
}

EXPORT void* CreateInterface(const char* pName, int* pReturnCode)
{
    if (serverCreateInterface == NULL)
    {
        DeleteLogFile();
        AssertInsecureParameterIsPresent();

        const char* gameDirectory = Plat_GetGameDirectory();
        gameInfoPath = string(gameDirectory) + "/csgo/gameinfo.gi";
        gameInfoBackupPath = string(gameDirectory) + "/csgo/gameinfo.gi.backup";
        string libPath = string(gameDirectory) + SERVER_LIB_PATH;

        void* serverModule = LoadLib(libPath.c_str());
        if (serverModule == NULL)
        {
            PluginError("Could not load server lib %s : %s", libPath.c_str(), GetLastErrorString());
        }

        serverCreateInterface = (CreateInterfaceFn)GetLibAddress(serverModule, "CreateInterface");
        if (serverCreateInterface == NULL)
        {
            PluginError("Could not find CreateInterface : %s", GetLastErrorString());
        }
    }

    void* original = serverCreateInterface(pName, pReturnCode);
    auto vtable = *(void***)original;
    if (strcmp(pName, "Source2ServerConfig001") == 0)
    {
        serverConfigConnect = (AppSystemConnectFn)vtable[0];
        serverConfigShutdown = (AppSystemShutdownFn)vtable[4];
        PatchVTableEntry(vtable, 0, (void*)&Connect);
        PatchVTableEntry(vtable, 4, (void*)&Shutdown);
    } else if (strcmp(pName, "Source2GameClients001") == 0)
    {
        originalClientFullyConnect = (ClientFullyConnectFn)vtable[15];
        PatchVTableEntry(vtable, 15, (void*)&NewClientFullyConnect);
    }

    if (demoPath == NULL) {
        int paramCount = CommandLine()->ParmCount();
        for (int i = 0; i < paramCount; i++) {
            const char* param = CommandLine()->GetParm(i);
            if (strcmp(param, "+playdemo") == 0 && i + 1 < paramCount) {
                demoPath = CommandLine()->GetParm(i + 1);
                LoadSequencesFile(string(demoPath));
                break;
            }
        }
    }

    return original;
}

#ifdef CON_COMMAND_ENABLED
CON_COMMAND(dem_render_info, "Prints dem-render plugin info")
{
    Log("Tick: %d", currentTick.load());
    Log("Is playing demo: %d", isPlayingDemo);

    std::lock_guard<std::mutex> lock(sequencesMutex);
    Log("Sequence count: %d", sequences.size());
}
#endif
