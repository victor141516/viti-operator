package com.viti.operator.auth;

interface IPrivilegedService {
    void destroy() = 16777114;
    String grantSimAuthentication(String packageName) = 1;
    String enableRemoteSimMode() = 2;
    String disableRemoteSimMode(String restoreNetworkMask) = 3;
}
