/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

"use client"

import {Box, Button, Grid, Typography, Skeleton, Alert, Collapse} from "@mui/material";
import React, {useEffect, useMemo, useState} from "react";

import {PendingCard, Card, NamespaceCardSkeleton, CreateNamespaceCard} from "@/components/Namespace";
import Link from "next/link";
import {Namespace, Alert as AlertType} from "@/components/Main";
import UnauthenticatedContent from "@/components/layout/UnauthenticatedContent";
import {Authenticated, getAuthenticated, isLoggedIn} from "@/helpers/login";


export default function Home() {

    const [data, setData] = useState<Namespace[] | undefined>(undefined);
    const [alert, setAlert] = useState<AlertType | undefined>(undefined)
    const [authenticated, setAuthenticated] = useState<Authenticated | undefined>(undefined)

    const getData = async () => {

        let data : Namespace[] = []

        const url = new URL("/api/v1.0/registry_ui/namespaces", window.location.origin)

        const response = await fetch(url)
        if (response.ok) {
            const responseData: Namespace[] = await response.json()
            responseData.sort((a, b) => a.id > b.id ? 1 : -1)
            responseData.forEach((namespace) => {
                if (namespace.prefix.startsWith("/cache")) {
                    namespace.type = "cache"
                } else {
                    namespace.type = "origin"
                }
            })
            data = responseData
        }

        return data
    }

    const _setData = async () => {setData(await getData())}

    useEffect(() => {
        _setData();
        (async () => {
            if(await isLoggedIn()){
                setAuthenticated(getAuthenticated() as Authenticated)
            }
        })();
    }, [])

    const pendingData = useMemo(
        () => data?.filter(
            (namespace) => namespace.admin_metadata.status === "Pending" &&
                (authenticated?.user == namespace.admin_metadata.user_id || authenticated?.role == "admin")
                ), [data, authenticated]
    )
    const approvedCacheData = useMemo(
        () => data?.filter(
            (namespace) => namespace.admin_metadata.status === "Approved" && namespace.type == "cache"
        ),
        [data]
    )
    const approvedOriginData = useMemo(
        () => data?.filter(
            (namespace) => namespace.admin_metadata.status === "Approved" && namespace.type == "origin"
        ),
        [data]
    )

    return (
        <Box width={"100%"}>
            <Grid container spacing={2}>
                <Grid item xs={12} lg={6} xl={4}>
                    <Typography variant={"h4"}>Namespace Registry</Typography>
                    <Collapse in={alert !== undefined}>
                        <Box mt={2}>
                            <Alert severity={alert?.severity}>{alert?.message}</Alert>
                        </Box>
                    </Collapse>
                </Grid>
                <Grid item lg={6} xl={8}>
                </Grid>
                <Grid item xs={12} lg={6} xl={4} justifyContent={"space-between"}>
                    <UnauthenticatedContent pb={2}>
                        <Typography variant={"body1"}>
                            Login to register new namespaces.
                            <Button sx={{ml:2}} variant={"contained"} size={"small"} color={"primary"} href={"/view/login/"}>Login</Button>
                        </Typography>
                    </UnauthenticatedContent>
                    {
                        pendingData && pendingData.length > 0 &&
                        <Grid item xs={12}>
                            <Typography variant={"h5"} pb={2}>Pending Registrations</Typography>
                            <Typography variant={"subtitle1"} pb={2}>
                                {authenticated !== undefined && authenticated?.role == "admin" && "Awaiting approval from you."}
                                {authenticated !== undefined && authenticated?.role != "admin" && "Awaiting approval from registry administrators."}
                            </Typography>

                            {pendingData.map((namespace) => <PendingCard key={namespace.id} namespace={namespace} authenticated={authenticated} onAlert={(a) => setAlert(a)} onUpdate={_setData}/>)}
                        </Grid>
                    }

                    <Typography variant={"h5"} py={2}>Public Namespaces</Typography>

                    <Typography variant={"subtitle1"}>
                        {authenticated !== undefined && authenticated?.role == "admin" &&
                            "As an administrator, you can edit Public Namespaces by click the pencil button"
                        }
                        {authenticated !== undefined && authenticated?.role != "admin" &&
                            "Public Namespaces are approved by the registry administrators. To edit a Namespace you own please contact the registry administrators."
                        }
                    </Typography>

                    <Typography variant={"h6"} py={2}>Origins</Typography>
                    { approvedOriginData !== undefined ? approvedOriginData.map((namespace) => <Card key={namespace.id} namespace={namespace} authenticated={authenticated}/>) : <NamespaceCardSkeleton/> }
                    { approvedOriginData !== undefined && approvedOriginData.length === 0 && <CreateNamespaceCard text={"Register Origin"}/>}

                    <Typography variant={"h6"} py={2}>Caches</Typography>
                    { approvedCacheData !== undefined ? approvedCacheData.map((namespace) => <Card key={namespace.id} namespace={namespace} authenticated={authenticated}/>) : <NamespaceCardSkeleton/> }
                    { approvedCacheData !== undefined && approvedCacheData.length === 0 && <CreateNamespaceCard text={"Register Cache"}/>}

                </Grid>
                <Grid item lg={6} xl={8}>
                </Grid>
            </Grid>
        </Box>
    )
}
